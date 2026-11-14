// Package relay is the WebSocket client to the Nudgebee relay. It
// connects to /register with Basic-Auth, sends an auth greeting, then
// loops reading inbound ExternalActionRequest envelopes and writing
// back Response envelopes.
//
// Reconnect strategy: after any read/write error or close, wait and dial
// again. The wait is random — anywhere from zero up to ReconnectDelay,
// doubling after each attempt that does not hold a session, up to
// MaxReconnectDelay — so the agents of a relay that restarts do not all
// redial it at the same instant and then retry in step. A session that
// stays up for MaxReconnectDelay resets the doubling.
//
// Liveness: a bare ReadMessage loop cannot detect a half-open connection.
// If the TCP connection dies without a FIN/RST reaching us (LB idle timeout,
// NAT table eviction, node network partition) the read blocks forever, the
// session never ends, and the reconnect loop above never runs — the agent
// looks healthy (pod Running, no restarts, no logs) while the relay has long
// since marked it NOT_CONNECTED. Only a pod restart recovered it.
//
// So the client both sends its own pings every PingInterval and holds a
// PongWait read deadline that any traffic from the relay — a pong, a ping, or
// a real message — pushes forward. A dead connection now surfaces as a read
// timeout within PongWait and reconnects like any other session error.
package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sync/semaphore"
)

// Handler is invoked for each inbound message. It receives the raw bytes
// (so dispatchers can decide which envelope to parse) and a function to
// send a response back over the WS. The handler should return quickly;
// long-running work belongs in a goroutine pool managed by the caller.
type Handler func(ctx context.Context, msg []byte, send SendFunc)

// SendFunc serialises and writes one Response over the WS. It is safe to
// call concurrently from multiple goroutines.
type SendFunc func(resp *Response) error

// Config configures the relay client.
type Config struct {
	URL            string        // ws://relay:8080/register
	AuthSecretKey  string        // NUDGEBEE_AUTH_SECRET_KEY (Basic-Auth)
	Greeting       Greeting      // sent on every (re)connect
	ReconnectDelay time.Duration // first reconnect waits up to this; default 3s
	// MaxReconnectDelay caps the reconnect wait as it doubles. Default 10s:
	// once the relay is back, no agent waits more than this to rejoin it.
	MaxReconnectDelay time.Duration
	WriteTimeout      time.Duration // default 30s
	// PingInterval is how often we send a WebSocket ping. Default 30s.
	PingInterval time.Duration
	// PongWait is the read deadline, refreshed on every frame the relay
	// sends us. Default 90s — deliberately several ping intervals wide, so a
	// CPU-starved node that delays our read goroutine drops a ping rather
	// than the whole session.
	PongWait time.Duration
	Logger   *slog.Logger
	// HandlerPoolSize is a soft outer bound on concurrent inbound-message
	// handler goroutines, guarding against goroutine pile-up when the
	// dispatcher's own pools are saturated. <=0 leaves it unbounded (the
	// historical behavior; used by tests). The dispatcher remains the
	// authoritative responder under normal load.
	HandlerPoolSize int
	// OnShed, if set, is called whenever a message is shed due to pool
	// saturation so main can bump a Prometheus counter.
	OnShed func()
	// OnConnect / OnDisconnect, if set, bracket each live WS session so main
	// can drive the relay_connected gauge. OnDisconnect runs on every exit
	// path out of an established session, so the gauge cannot latch at 1.
	OnConnect    func()
	OnDisconnect func()
	// OnReconnect, if set, is called before each redial after the first, so
	// main can count reconnect attempts.
	OnReconnect func()
}

// Client manages the WebSocket lifecycle. One Client per agent process.
type Client struct {
	cfg     Config
	handler Handler

	mu   sync.Mutex // protects conn for concurrent writers
	conn *websocket.Conn

	sem  *semaphore.Weighted // nil = unbounded
	shed atomic.Uint64

	// connectedAt is when the current session was established; zero
	// while dialing. Written by runOnce and read by Run, on one goroutine.
	connectedAt time.Time
}

func NewClient(cfg Config, h Handler) *Client {
	if cfg.ReconnectDelay == 0 {
		cfg.ReconnectDelay = 3 * time.Second
	}
	if cfg.MaxReconnectDelay == 0 {
		cfg.MaxReconnectDelay = 10 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 30 * time.Second
	}
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.PongWait == 0 {
		cfg.PongWait = 90 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	c := &Client{cfg: cfg, handler: h}
	if cfg.HandlerPoolSize > 0 {
		c.sem = semaphore.NewWeighted(int64(cfg.HandlerPoolSize))
	}
	return c
}

// Shed returns the count of inbound messages dropped because the handler pool
// was saturated. Wire to a Prometheus counter in main.
func (c *Client) Shed() uint64 { return c.shed.Load() }

// Run blocks, dialing the relay and reconnecting until ctx is cancelled.
func (c *Client) Run(ctx context.Context) error {
	attempt := 0 // reconnects since the last session that held
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.connectedAt = time.Time{}
		err := c.runOnce(ctx)
		if !c.connectedAt.IsZero() && time.Since(c.connectedAt) >= c.cfg.MaxReconnectDelay {
			attempt = 0
		}
		wait := reconnectWait(c.cfg.ReconnectDelay, c.cfg.MaxReconnectDelay, attempt, rand.Int64N)
		attempt++
		if err != nil {
			c.cfg.Logger.Warn("relay session ended", "err", err, "reconnect_in", wait.Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
			if c.cfg.OnReconnect != nil {
				c.cfg.OnReconnect()
			}
		}
	}
}

// reconnectWait is how long to wait before reconnect number attempt
// (0-based): a uniformly random duration up to base doubled attempt times,
// capped at limit ("full jitter"). randN returns a value in [0, n).
func reconnectWait(base, limit time.Duration, attempt int, randN func(n int64) int64) time.Duration {
	ceiling := min(base, limit)
	for i := 0; i < attempt && ceiling < limit; i++ {
		if ceiling > limit/2 { // doubling would pass limit, or overflow
			ceiling = limit
			break
		}
		ceiling *= 2
	}
	if ceiling <= 0 {
		return 0
	}
	if ceiling == math.MaxInt64 {
		return time.Duration(randN(math.MaxInt64))
	}
	return time.Duration(randN(int64(ceiling) + 1))
}

func (c *Client) runOnce(ctx context.Context) error {
	header := http.Header{}
	encoded := base64.StdEncoding.EncodeToString([]byte(c.cfg.AuthSecretKey))
	header.Set("Authorization", encoded)

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, c.cfg.URL, header)
	if resp != nil {
		// gorilla/websocket returns the HTTP handshake response on both
		// success and failure paths; close its Body to avoid the leak.
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial %s: %w (status %d)", c.cfg.URL, err, resp.StatusCode)
		}
		return fmt.Errorf("dial %s: %w", c.cfg.URL, err)
	}
	c.connectedAt = time.Now()
	c.cfg.Logger.Info("relay connected", "url", c.cfg.URL)
	if c.cfg.OnConnect != nil {
		c.cfg.OnConnect()
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		_ = c.conn.Close()
		c.conn = nil
		c.mu.Unlock()
		if c.cfg.OnDisconnect != nil {
			c.cfg.OnDisconnect()
		}
	}()

	// Keepalive. Every handler below runs on this goroutine (gorilla calls
	// them from ReadMessage), so SetReadDeadline here is safe.
	resetDeadline := func() {
		_ = conn.SetReadDeadline(time.Now().Add(c.cfg.PongWait))
	}
	resetDeadline()
	conn.SetPongHandler(func(string) error {
		resetDeadline()
		return nil
	})
	// Replaces gorilla's default ping handler, which replies with a pong but
	// does not refresh the read deadline. The relay pings every 30s, so its
	// pings alone keep an idle session alive.
	conn.SetPingHandler(func(appData string) error {
		resetDeadline()
		err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(c.cfg.WriteTimeout))
		if errors.Is(err, websocket.ErrCloseSent) {
			return nil
		}
		if socketWriteFailed(err) {
			return err
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			// Timed out waiting behind a slow response write, not on the
			// socket: the connection is fine, the pong is just late.
			return nil
		}
		return err
	})

	// Pinger. Bound to this session: cancelled and joined before the
	// deferred Close above runs, so it never writes to a closed conn.
	sessionCtx, endSession := context.WithCancel(ctx)
	var pinger sync.WaitGroup
	pinger.Add(2)
	go func() {
		defer pinger.Done()
		c.pingLoop(sessionCtx, conn)
	}()
	// Shed replies go through one worker and a bounded queue so a flood of shed
	// messages cannot pile up goroutines blocked on the write lock.
	shedQ := make(chan string, shedReplyQueue)
	go func() {
		defer pinger.Done()
		c.shedReplyLoop(sessionCtx, shedQ)
	}()
	defer func() {
		endSession()
		pinger.Wait()
	}()

	if err := c.send(&Greeting{
		Action:          "auth",
		Version:         c.cfg.Greeting.Version,
		AgentVersion:    c.cfg.Greeting.AgentVersion,
		AgentCommit:     c.cfg.Greeting.AgentCommit,
		AgentBuildTime:  c.cfg.Greeting.AgentBuildTime,
		ProtocolVersion: c.cfg.Greeting.ProtocolVersion,
	}); err != nil {
		return fmt.Errorf("send greeting: %w", err)
	}

	send := SendFunc(func(r *Response) error { return c.send(r) })

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		resetDeadline()
		if c.sem == nil {
			go c.handler(sessionCtx, msg, send)
			continue
		}
		if !c.sem.TryAcquire(1) {
			// Pool saturated — shed rather than pile up unbounded goroutines.
			// Answer 503 so the caller fails fast instead of waiting out its timeout.
			c.shed.Add(1)
			if c.cfg.OnShed != nil {
				c.cfg.OnShed()
			}
			c.cfg.Logger.Warn("relay handler pool saturated; shedding message", "shed_total", c.shed.Load())
			c.enqueueShedReply(shedQ, msg)
			continue
		}
		go func() {
			defer c.sem.Release(1)
			c.handler(sessionCtx, msg, send)
		}()
	}
}

// shedReplyQueue bounds pending 503 replies per session; beyond it the reply is
// dropped and the caller falls back to its own timeout.
const shedReplyQueue = 256

// enqueueShedReply queues a 503 reply for a shed message so the caller does not
// block until its own timeout. Messages without a request_id are
// fire-and-forget. Never blocks the read loop.
func (c *Client) enqueueShedReply(q chan<- string, msg []byte) {
	var probe struct {
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(msg, &probe) != nil || probe.RequestID == "" {
		return
	}
	select {
	case q <- probe.RequestID:
	default:
	}
}

func (c *Client) shedReplyLoop(ctx context.Context, q <-chan string) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-q:
			if err := c.send(&Response{
				Action:     "response",
				RequestID:  id,
				StatusCode: 503,
				Data:       map[string]any{"error": "agent overloaded"},
				OutputType: "actions",
			}); err != nil {
				c.cfg.Logger.Warn("relay shed reply failed", "err", err)
			}
		}
	}
}

// pingLoop sends a ping every PingInterval until the session ends. A ping
// that fails on the socket closes the connection, which ends the read loop and
// the session. Waiting for the read loop to notice is not enough: a relay that
// has stopped reading can still be sending us pings, and each one pushes the
// read deadline forward, so the session would never end.
//
// WriteControl is safe to call concurrently with the WriteMessage in send(),
// so this deliberately does not take c.mu — a pinger blocked behind a slow
// response write is a pinger that cannot do its job.
func (c *Client) pingLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(c.cfg.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(c.cfg.WriteTimeout)); err != nil {
				c.cfg.Logger.Warn("relay ping failed", "err", err)
				if socketWriteFailed(err) {
					_ = conn.Close()
				}
				return
			}
		}
	}
}

// send writes one JSON-encoded value over the WS. Locks to serialise writes,
// since gorilla/websocket forbids concurrent WriteMessage calls.
func (c *Client) send(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("relay: not connected")
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.cfg.WriteTimeout)); err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	err = c.conn.WriteMessage(websocket.TextMessage, data)
	if socketWriteFailed(err) {
		// gorilla refuses every later write on this connection, so the session
		// is finished; close it so the read loop ends and Run redials. See
		// pingLoop for why the read loop alone may never notice.
		c.cfg.Logger.Warn("relay write failed; closing session", "err", err)
		_ = c.conn.Close()
	}
	return err
}

// socketWriteFailed reports whether err came from writing to the socket itself
// (a timeout because the relay stopped reading, a reset, a broken pipe), as
// opposed to gorilla giving up while waiting for another writer to finish.
func socketWriteFailed(err error) bool {
	var op *net.OpError
	return errors.As(err, &op)
}
