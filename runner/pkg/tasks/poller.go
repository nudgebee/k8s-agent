// Package tasks polls /v1/k8s/tasks for queued agent jobs (krr_scan, popeye_scan,
// image_scanner, k8s_version_upgrade, helm_chart_upgrade, certificate_scanner,
// trivy_cis_scan, kube_bench_scan, …) and dispatches them through the local
// handler registry, then POSTs the result back to /v1/k8s/tasks/<task_id>.
//
// Without this loop, no recommendation jobs run for the tenant — api-server's
// CreateRecommendationJob writes rows to the `agent_task` table with status=TODO
// , and the agent never picks
// them up. This is the primary reason the canary saw zero recommendations.
//
// The collector returns:
//
//	{ "data": [ {"task_id": "...", "payload": {action_name, action_params, ...}, "source": "..."} ],
//	  "remaining_task_count": <int> }
//
// We drain the queue in a tight loop while remaining_task_count > 0, then
// sleep Period seconds before the next pull (default 120s — TASK_RUNNER_WINDOW
// env var).
package tasks

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Dispatcher is the subset of dispatch.Dispatcher we need — kept narrow so
// tests don't need to construct the full thing.
type Dispatcher interface {
	HandleTrusted(ctx context.Context, actionName string, params map[string]any, highPriority bool) (any, bool, error)
}

// Service drains the agent_task queue. One Service per agent process.
type Service struct {
	Endpoint   string // e.g. https://collector.dev.nudgebee.pollux.in
	AuthSecret string // bare base64 same as discovery sink
	Period     time.Duration
	HTTP       *http.Client
	Logger     *slog.Logger
	Dispatch   Dispatcher

	// LongActions names actions that may run for many minutes (the
	// rightsize_pvc downsize migration). They are dispatched on a separate
	// goroutine so a single slow task doesn't stall the sequential queue
	// drain. The collector already flipped the row to PROCESSING on GET, so it
	// is not re-handed while running. On shutdown the agent context is
	// cancelled, so an in-flight migration aborts and its rollback defers run
	// within the termination grace; a hard kill still orphans the task (it
	// reaps to TIMEOUT, recoverable by re-applying).
	LongActions map[string]struct{}
}

// Run blocks until ctx is done. Polls in a loop with Period spacing.
func (s *Service) Run(ctx context.Context) error {
	if s.Endpoint == "" {
		s.Logger.Info("task poller disabled — backend endpoint empty")
		<-ctx.Done()
		return nil
	}
	if s.HTTP == nil {
		s.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	if s.Period <= 0 {
		s.Period = 120 * time.Second
	}
	t := time.NewTimer(s.Period) // first tick after Period; agent_task is empty on fresh boot
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := s.drain(ctx); err != nil && !isContextErr(err) {
				s.Logger.Warn("task poller drain failed", "err", err)
			}
			t.Reset(s.Period)
		}
	}
}

// drain pulls all available tasks (looping while remaining_task_count > 0)
// and processes each one.
func (s *Service) drain(ctx context.Context) error {
	if s.HTTP == nil {
		// Defensive — Run() also initializes this, but tests call drain
		// directly. Either path produces the same client.
		s.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	for {
		batch, remaining, err := s.fetch(ctx)
		if err != nil {
			return err
		}
		for _, t := range batch {
			s.process(ctx, t)
		}
		if remaining == 0 {
			return nil
		}
	}
}

// task is the row shape collector returns. `payload` may itself be a JSON
// string (the api-server inserts it as a string into the column) or a map —
// accommodate both.
type task struct {
	TaskID  string          `json:"task_id"`
	Payload json.RawMessage `json:"payload"`
	Source  string          `json:"source"`
}

func (s *Service) fetch(ctx context.Context) ([]task, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Endpoint+"/v1/k8s/tasks", nil)
	if err != nil {
		return nil, 0, err
	}
	if s.AuthSecret != "" {
		req.Header.Set("Authorization", base64.StdEncoding.EncodeToString([]byte(s.AuthSecret)))
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode >= 400 {
		return nil, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 256))
	}
	var envelope struct {
		Data      []task `json:"data"`
		Remaining int    `json:"remaining_task_count"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, 0, fmt.Errorf("decode tasks: %w", err)
	}
	return envelope.Data, envelope.Remaining, nil
}

// process unpacks one task's payload to {action_name, action_params}, runs
// it through the trusted dispatcher, and posts the result back.
func (s *Service) process(ctx context.Context, t task) {
	payload, err := unpackPayload(t.Payload)
	if err != nil {
		s.Logger.Warn("task: payload decode failed", "task_id", t.TaskID, "err", err)
		s.respond(ctx, t.TaskID, map[string]any{"success": false, "msg": "invalid payload: " + err.Error()})
		return
	}
	actionName, _ := payload["action_name"].(string)
	if actionName == "" {
		s.Logger.Warn("task: missing action_name", "task_id", t.TaskID)
		s.respond(ctx, t.TaskID, map[string]any{"success": false, "msg": "payload missing action_name"})
		return
	}
	params, _ := payload["action_params"].(map[string]any)

	if _, long := s.LongActions[actionName]; long {
		// Run off the drain goroutine so a multi-minute task doesn't stall the
		// rest of the queue. We keep the agent context (it's the long-lived
		// lifecycle ctx — the poll loop reuses it, so only shutdown cancels it)
		// so a graceful shutdown propagates cancellation and the migration's
		// rollback defers can clean up within the termination grace.
		taskID, action, p := t.TaskID, actionName, params
		go s.runTask(ctx, taskID, action, p)
		return
	}
	s.runTask(ctx, t.TaskID, actionName, params)
}

// runTask dispatches one action through the trusted handler path and posts the
// result back. Shared by the synchronous (short) and detached (long) paths.
func (s *Service) runTask(ctx context.Context, taskID, actionName string, params map[string]any) {
	start := time.Now()
	data, ok, err := s.Dispatch.HandleTrusted(ctx, actionName, params, false)
	elapsed := time.Since(start)

	var resp map[string]any
	switch {
	case !ok:
		s.Logger.Warn("task: action not registered", "task_id", taskID, "action_name", actionName)
		resp = map[string]any{"success": false, "msg": "action not registered: " + actionName}
	case err != nil:
		s.Logger.Warn("task: handler error", "task_id", taskID, "action_name", actionName, "err", err)
		resp = map[string]any{"success": false, "msg": err.Error()}
	default:
		// Handlers already return {success, ...} for thin actions; for
		// Finding-shape actions they return the full {success, findings,...}
		// dict. Accept both — the collector's save_task_status reads
		// data["success"] and stores the rest as response JSON.
		switch d := data.(type) {
		case map[string]any:
			resp = d
			if _, has := resp["success"]; !has {
				resp["success"] = true
			}
		default:
			resp = map[string]any{"success": true, "data": d}
		}
	}
	resp["task_processing_duration"] = int(elapsed.Round(time.Second).Seconds())
	s.respond(ctx, taskID, resp)
}

// respond posts the task result back to /v1/k8s/tasks/<task_id>.
func (s *Service) respond(ctx context.Context, taskID string, body map[string]any) {
	buf, err := json.Marshal(body)
	if err != nil {
		s.Logger.Warn("task: marshal response failed", "task_id", taskID, "err", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint+"/v1/k8s/tasks/"+taskID, bytes.NewReader(buf))
	if err != nil {
		s.Logger.Warn("task: build response request failed", "task_id", taskID, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if s.AuthSecret != "" {
		req.Header.Set("Authorization", base64.StdEncoding.EncodeToString([]byte(s.AuthSecret)))
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		s.Logger.Warn("task: post response failed", "task_id", taskID, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		s.Logger.Warn("task: post response HTTP error", "task_id", taskID, "status", resp.StatusCode, "body", string(respBody))
	}
}

// unpackPayload accepts a JSON string OR a JSON object — the api-server
// inserts the payload column as a string for some actions and as an object
// for others (history; see CreateRecommendationJob vs older code paths).
func unpackPayload(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty payload")
	}
	// Try object first.
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj, nil
	}
	// Fall back: it's a JSON-encoded string containing a JSON object.
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, fmt.Errorf("payload string is not valid JSON: %w", err)
	}
	return obj, nil
}

func isContextErr(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ParseTaskWindow reads TASK_RUNNER_WINDOW env (seconds) and returns the
// poll period. Used by main.go.
func ParseTaskWindow(s string) time.Duration {
	if s == "" {
		return 120 * time.Second
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 120 * time.Second
	}
	return time.Duration(n) * time.Second
}
