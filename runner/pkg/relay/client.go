// Package relay is the WebSocket client to the Nudgebee relay. It
// connects to /register with Basic-Auth, sends an auth greeting, then
// loops reading inbound ExternalActionRequest envelopes and writing
// back Response envelopes.
//
// Reconnect strategy: after any read/write error or close, sleep
// ReconnectDelay seconds and dial again. Matches the legacy
// run_forever loop.
package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
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
	Logger         *slog.Logger
}

// Client manages the WebSocket lifecycle. One Client per agent process.
type Client struct {
	cfg     Config
	handler Handler

	mu   sync.Mutex // protects conn for concurrent writers
	conn *websocket.Conn
}

func NewClient(cfg Config, h Handler) *Client {
	if cfg.ReconnectDelay == 0 {
		cfg.ReconnectDelay = 3 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Client{cfg: cfg, handler: h}
}

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
		go c.handler(ctx, msg, send)
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
