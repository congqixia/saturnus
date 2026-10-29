package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"saturnus/internal/workerproto"
)

// ClientConfig configures a worker's control-channel client.
type ClientConfig struct {
	// ServerURL is the saturnus HTTP(S) base URL.
	ServerURL string
	WorkerID  string
	Token     string
	// InFlight returns the review IDs the worker is currently running (sent in
	// the hello frame on connect/reconnect so the server can reconcile).
	InFlight func() []string
	// Heartbeat returns the load the worker reports periodically.
	Heartbeat func() workerproto.HeartbeatPayload
	// OnCommand handles server->worker frames (claim/cancel/ping).
	OnCommand func(ctx context.Context, frame workerproto.Frame) error
}

// Client is a worker's outbound WebSocket control connection with automatic
// reconnect and heartbeat.
type Client struct {
	cfg    ClientConfig
	mu     sync.Mutex
	ws     *websocket.Conn
	closed bool
}

// NewClient builds a control-channel client.
func NewClient(cfg ClientConfig) *Client {
	return &Client{cfg: cfg}
}

// Run maintains the connection until ctx is cancelled, reconnecting with
// exponential backoff on failure.
func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		err := c.connectOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		log.Printf("saturnus-worker: control channel disconnected (%v); reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// Send pushes an event frame to the server over the current connection.
func (c *Client) Send(frame workerproto.Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ws == nil {
		return errors.New("control channel not connected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return c.ws.Write(ctx, websocket.MessageText, data)
}

// Close terminates the current connection and stops reconnecting.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	ws := c.ws
	c.ws = nil
	c.mu.Unlock()
	if ws != nil {
		_ = ws.Close(websocket.StatusNormalClosure, "shutting down")
	}
}

func (c *Client) connectOnce(ctx context.Context) error {
	u := c.wsURL()
	ws, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		return err
	}
	ws.SetReadLimit(workerproto.MaxMessageBytes)
	c.mu.Lock()
	c.ws = ws
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.ws == ws {
			c.ws = nil
		}
		c.mu.Unlock()
		_ = ws.Close(websocket.StatusNormalClosure, "done")
	}()

	// Announce the connection and the jobs we are still running.
	if err := c.Send(workerproto.NewFrame(workerproto.EvHello, "", workerproto.HelloPayload{
		WorkerID: c.cfg.WorkerID,
		Token:    c.cfg.Token,
		InFlight: c.cfg.InFlight(),
	})); err != nil {
		return err
	}

	heartbeatCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.heartbeatLoop(heartbeatCtx)

	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		var frame workerproto.Frame
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		switch frame.Type {
		case workerproto.CmdPing:
			hb := workerproto.HeartbeatPayload{}
			if c.cfg.Heartbeat != nil {
				hb = c.cfg.Heartbeat()
			}
			_ = c.Send(workerproto.NewFrame(workerproto.EvHeartbeat, "", hb))
		default:
			if c.cfg.OnCommand != nil {
				if err := c.cfg.OnCommand(ctx, frame); err != nil {
					log.Printf("saturnus-worker: handle %s: %v", frame.Type, err)
				}
			}
		}
	}
}

func (c *Client) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			hb := workerproto.HeartbeatPayload{}
			if c.cfg.Heartbeat != nil {
				hb = c.cfg.Heartbeat()
			}
			if err := c.Send(workerproto.NewFrame(workerproto.EvHeartbeat, "", hb)); err != nil {
				return
			}
		}
	}
}

func (c *Client) wsURL() string {
	base := strings.TrimRight(c.cfg.ServerURL, "/")
	base = strings.Replace(base, "http://", "ws://", 1)
	base = strings.Replace(base, "https://", "wss://", 1)
	return fmt.Sprintf("%s/api/workers/ws?worker_id=%s&token=%s",
		base, url.QueryEscape(c.cfg.WorkerID), url.QueryEscape(c.cfg.Token))
}
