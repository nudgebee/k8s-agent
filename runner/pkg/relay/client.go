// Package relay is the WebSocket client to the Nudgebee relay. It
// connects to /register with Basic-Auth, sends an auth greeting, then
// loops reading inbound ExternalActionRequest envelopes and writing
// back Response envelopes.
//
// Reconnect strategy: after any read/write error or close, sleep
// ReconnectDelay seconds and dial again. Matches the legacy
// run_forever loop.
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
	ReconnectDelay time.Duration // default 3s
	WriteTimeout   time.Duration // default 30s
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
}

// Client manages the WebSocket lifecycle. One Client per agent process.
type Client struct {
	cfg     Config
	handler Handler

	mu   sync.Mutex // protects conn for concurrent writers
	conn *websocket.Conn

	sem  *semaphore.Weighted // nil = unbounded
	shed atomic.Uint64
}

func NewClient(cfg Config, h Handler) *Client {
	if cfg.ReconnectDelay == 0 {
		cfg.ReconnectDelay = 3 * time.Second
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
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.runOnce(ctx); err != nil {
			c.cfg.Logger.Warn("relay session ended", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.cfg.ReconnectDelay):
		}
	}
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
	c.cfg.Logger.Info("relay connected", "url", c.cfg.URL)

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		_ = c.conn.Close()
		c.conn = nil
		c.mu.Unlock()
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
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil
		}
		return err
	})

	// Pinger. Bound to this session: cancelled and joined before the
	// deferred Close above runs, so it never writes to a closed conn.
	sessionCtx, endSession := context.WithCancel(ctx)
	var pinger sync.WaitGroup
	pinger.Add(1)
	go func() {
		defer pinger.Done()
		c.pingLoop(sessionCtx, conn)
	}()
	defer func() {
		endSession()
		pinger.Wait()
	}()

	if err := c.send(&Greeting{
		Action:         "auth",
		Version:        c.cfg.Greeting.Version,
		AgentVersion:   c.cfg.Greeting.AgentVersion,
		AgentCommit:    c.cfg.Greeting.AgentCommit,
		AgentBuildTime: c.cfg.Greeting.AgentBuildTime,
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
			go c.handler(ctx, msg, send)
			continue
		}
		if !c.sem.TryAcquire(1) {
			// Pool saturated — shed rather than pile up unbounded goroutines.
			// The backend will time out / retry; better than OOM under overload.
			c.shed.Add(1)
			if c.cfg.OnShed != nil {
				c.cfg.OnShed()
			}
			c.cfg.Logger.Warn("relay handler pool saturated; shedding message", "shed_total", c.shed.Load())
			continue
		}
		go func() {
			defer c.sem.Release(1)
			c.handler(ctx, msg, send)
		}()
	}
}

// pingLoop sends a ping every PingInterval until the session ends. A failed
// write means the connection is gone; returning unblocks nothing by itself,
// but the read loop sees the same failure (or times out on PongWait) and ends
// the session.
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
	return c.conn.WriteMessage(websocket.TextMessage, data)
}
