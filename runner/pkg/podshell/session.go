// Package podshell implements the interactive pod-shell action surface
// the UI uses for the "open shell to pod" feature.
//
// Wire shape (relay → agent, message goes through WS as a TerminalRequest):
//
//	{ action: "start"|"exec"|"read"|"close",
//	  session_id, name, namespace, command, request_id }
//
// Wire shape (agent → relay, framed in AgentResponse with output_type=Terminal):
//
//	{ session_id, data, exit }
//	(plus "status" on `exec` and "error" on failures)
//
// Session lifecycle:
//
//	start  — open a kubectl-exec stream to <namespace>/<name>, return new
//	         session_id; reader goroutine drains stdout/stderr into a buffer.
//	exec   — write raw bytes (typically a line ending in \r) to the
//	         remote stdin. "exit" closes the session.
//	read   — drain the output buffer; UI polls this periodically.
//	close  — kill the exec stream and free the session.
//
// Idle sessions (no exec/read for IdleTimeout) are reaped by a background
// goroutine — matches the legacy session_cleanup behaviour.
//
// SECURITY: session.start opens an unfiltered shell inside a customer-cluster
// pod. RBAC for the agent's service account is the only gate; per-call
// audit log includes session_id + namespace + pod.
package podshell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// IdleTimeout is the max time a session can sit without exec/read before
// the cleanup goroutine reaps it. Matches the backend default.
const IdleTimeout = 10 * time.Minute

// CleanupInterval is how often the reaper sweeps.
const CleanupInterval = 1 * time.Minute

// Manager owns the live-sessions map and the reaper goroutine.
type Manager struct {
	cs      kubernetes.Interface
	restCfg *rest.Config

	mu       sync.Mutex
	sessions map[string]*session

	idleTimeout time.Duration
}

// NewManager returns a Manager with default timeouts. Caller must call
// Run(ctx) once to start the cleanup goroutine.
func NewManager(cs kubernetes.Interface, restCfg *rest.Config) *Manager {
	return &Manager{
		cs:          cs,
		restCfg:     restCfg,
		sessions:    map[string]*session{},
		idleTimeout: IdleTimeout,
	}
}

// Run blocks until ctx is done, sweeping idle sessions every CleanupInterval.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(CleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.closeAll()
			return
		case <-t.C:
			m.reap()
		}
	}
}

// Request is the wire-shape of a TerminalRequest received from the relay.
type Request struct {
	Action    string `json:"action"`
	SessionID string `json:"session_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Command   string `json:"command,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// Response is the inner shape of the agent's reply. The dispatcher
// JSON-stringifies this and puts it in AgentResponse.Data with
// OutputType="Terminal".
type Response struct {
	SessionID string `json:"session_id"`
	Data      string `json:"data"`
	Exit      bool   `json:"exit"`
	// Status is set only by exec — matches handle_exec at
	//
	Status string `json:"status,omitempty"`
	// Error is set when a request can't be satisfied (missing fields,
	// unknown session, etc.). Matches handle_*'s {"error": "..."} returns.
	Error string `json:"error,omitempty"`
}

// Handle dispatches a TerminalRequest. Returns the response + an HTTP
// status the dispatcher uses for the AgentResponse envelope (200 for ok,
// 400 for bad input).
func (m *Manager) Handle(ctx context.Context, r *Request) (*Response, int) {
	switch r.Action {
	case "start":
		return m.handleStart(ctx, r)
	case "exec":
		return m.handleExec(ctx, r)
	case "read":
		return m.handleRead(r)
	case "close":
		return m.handleClose(r)
	default:
		return &Response{Error: "Invalid action"}, 400
	}
}

func (m *Manager) handleStart(ctx context.Context, r *Request) (*Response, int) {
	if r.Name == "" || r.Namespace == "" {
		return &Response{Error: "name and namespace required"}, 400
	}
	if m.cs == nil || m.restCfg == nil {
		return &Response{Error: "agent has no K8s client; pod_shell unavailable"}, 503
	}
	sess, err := m.openSession(ctx, r.Namespace, r.Name)
	if err != nil {
		return &Response{Error: err.Error()}, 500
	}
	return &Response{SessionID: sess.id, Data: "", Exit: false}, 200
}

func (m *Manager) handleExec(_ context.Context, r *Request) (*Response, int) {
	if r.SessionID == "" {
		return &Response{Error: "session_id required"}, 400
	}
	sess := m.get(r.SessionID)
	if sess == nil {
		// Session expired/closed — return Exit=true so the UI reconnects.
		return &Response{SessionID: r.SessionID, Exit: true}, 200
	}
	// "exit" terminates the session.
	trimmed := strings.TrimSpace(r.Command)
	if trimmed == "exit" {
		m.dropAndClose(r.SessionID)
		return &Response{SessionID: r.SessionID, Exit: true}, 200
	}
	ok := sess.write(r.Command)
	status := "accepted"
	if !ok {
		status = "busy"
	}
	return &Response{SessionID: r.SessionID, Status: status, Exit: false}, 200
}

func (m *Manager) handleRead(r *Request) (*Response, int) {
	if r.SessionID == "" {
		return &Response{Error: "session_id required"}, 400
	}
	sess := m.get(r.SessionID)
	if sess == nil {
		return &Response{SessionID: r.SessionID, Exit: true}, 200
	}
	data, closed := sess.drain()
	return &Response{SessionID: r.SessionID, Data: data, Exit: closed}, 200
}

func (m *Manager) handleClose(r *Request) (*Response, int) {
	if r.SessionID == "" {
		return &Response{Error: "session_id required"}, 400
	}
	m.dropAndClose(r.SessionID)
	return &Response{SessionID: r.SessionID, Exit: true}, 200
}

// session is one live exec stream. Output bytes from stdout+stderr are
// merged into outBuf; reads drain it. write() pipes user input into
// stdinW. closed flips true when the reader sees EOF or close() is called.
type session struct {
	id        string
	namespace string
	name      string

	stdinW *io.PipeWriter
	cancel context.CancelFunc

	bufMu  sync.Mutex
	outBuf bytes.Buffer
	closed bool

	lastUsed time.Time
	usedMu   sync.Mutex
}

func (m *Manager) openSession(ctx context.Context, ns, name string) (*session, error) {
	// Best-effort wait for the pod to be Running.
	// retries up to 20s. We do the same with a 10s budget — anything longer
	// blocks the agent's WS reader.
	if err := m.waitRunning(ctx, ns, name); err != nil {
		return nil, err
	}

	stdinR, stdinW := io.Pipe()
	streamCtx, cancel := context.WithCancel(context.Background())
	s := &session{
		id:        uuid.NewString(),
		namespace: ns,
		name:      name,
		stdinW:    stdinW,
		cancel:    cancel,
		lastUsed:  time.Now(),
	}

	restReq := m.cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(name).
		Namespace(ns).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: []string{"/bin/sh"},
			Stdin:   true,
			Stdout:  true,
			Stderr:  true,
			TTY:     true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(m.restCfg, "POST", restReq.URL())
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		cancel()
		return nil, fmt.Errorf("build SPDY executor: %w", err)
	}

	// Background streamer: pumps the PTY. Output goes via outWriter into
	// outBuf; remote stdin is read from the PipeReader paired with stdinW.
	go func() {
		defer cancel()
		ow := &outWriter{s: s}
		err := exec.StreamWithContext(streamCtx, remotecommand.StreamOptions{
			Stdin:  stdinR,
			Stdout: ow,
			Stderr: ow,
			Tty:    true,
		})
		s.bufMu.Lock()
		s.closed = true
		if err != nil && !errors.Is(err, context.Canceled) {
			s.outBuf.WriteString("\n[stream closed: " + err.Error() + "]\n")
		}
		s.bufMu.Unlock()
		_ = stdinR.Close()
	}()

	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()
	return s, nil
}

func (m *Manager) waitRunning(ctx context.Context, ns, name string) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		pod, err := m.cs.CoreV1().Pods(ns).Get(cctx, name, metav1.GetOptions{})
		if err == nil && pod.Status.Phase == corev1.PodRunning {
			return nil
		}
		select {
		case <-cctx.Done():
			if err != nil {
				return fmt.Errorf("pod %s/%s not reachable: %w", ns, name, err)
			}
			return fmt.Errorf("pod %s/%s not Running", ns, name)
		case <-t.C:
		}
	}
}

func (m *Manager) get(id string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *Manager) dropAndClose(id string) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if ok {
		s.close()
	}
}

func (m *Manager) reap() {
	now := time.Now()
	m.mu.Lock()
	expired := []*session{}
	for id, s := range m.sessions {
		s.usedMu.Lock()
		idle := now.Sub(s.lastUsed)
		s.usedMu.Unlock()
		if idle > m.idleTimeout {
			delete(m.sessions, id)
			expired = append(expired, s)
		}
	}
	m.mu.Unlock()
	for _, s := range expired {
		s.close()
	}
}

func (m *Manager) closeAll() {
	m.mu.Lock()
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.sessions = map[string]*session{}
	m.mu.Unlock()
	for _, s := range all {
		s.close()
	}
}

// outWriter funnels stream stdout+stderr into the session's outBuf.
type outWriter struct{ s *session }

func (w *outWriter) Write(p []byte) (int, error) {
	w.s.bufMu.Lock()
	w.s.outBuf.Write(p)
	w.s.bufMu.Unlock()
	w.s.touch()
	return len(p), nil
}

func (s *session) write(input string) bool {
	if s == nil || s.stdinW == nil {
		return false
	}
	s.bufMu.Lock()
	closed := s.closed
	s.bufMu.Unlock()
	if closed {
		return false
	}
	if _, err := s.stdinW.Write([]byte(input)); err != nil {
		return false
	}
	s.touch()
	return true
}

func (s *session) drain() (string, bool) {
	s.bufMu.Lock()
	out := s.outBuf.String()
	s.outBuf.Reset()
	closed := s.closed
	s.bufMu.Unlock()
	s.touch()
	return out, closed
}

func (s *session) touch() {
	s.usedMu.Lock()
	s.lastUsed = time.Now()
	s.usedMu.Unlock()
}

func (s *session) close() {
	s.bufMu.Lock()
	s.closed = true
	s.bufMu.Unlock()
	if s.stdinW != nil {
		_ = s.stdinW.Close()
	}
	if s.cancel != nil {
		s.cancel()
	}
}
