// Package worker implements the WebSocket control channel between the saturnus
// server (Hub) and remote review workers (Client). A worker keeps one outbound
// connection; the server pushes claim/cancel commands and receives hello,
// heartbeat, progress and complete events.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"saturnus/internal/workerproto"
)

// Handler is the server-side consumer of worker lifecycle and event frames. It
// is typically implemented by the scheduler.
type Handler interface {
	// OnConnect is called after a worker has been authenticated.
	OnConnect(workerID string)
	// OnDisconnect is called when a worker's connection drops.
	OnDisconnect(workerID string)
	// OnEvent receives every non-connection frame from a worker.
	OnEvent(ctx context.Context, workerID string, frame workerproto.Frame) error
}

// AuthFunc validates a worker's token during connection establishment.
type AuthFunc func(workerID, token string) bool

// Hub authenticates and tracks worker WebSocket connections, tracks per-worker
// in-flight jobs, and routes frames to a Handler.
type Hub struct {
	mu       sync.Mutex
	conns    map[string]*Conn
	inFlight map[string]map[string]bool
	online   map[string]time.Time
	handler  Handler
	auth     AuthFunc
	closed   bool
}

// NewHub builds a Hub. handler receives events; auth validates workers.
func NewHub(handler Handler, auth AuthFunc) *Hub {
	return &Hub{
		conns:    map[string]*Conn{},
		inFlight: map[string]map[string]bool{},
		online:   map[string]time.Time{},
		handler:  handler,
		auth:     auth,
	}
}

// ServeWS upgrades an HTTP request into a worker connection. The worker is
// identified and authenticated via the worker_id and token query parameters.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) error {
	workerID := r.URL.Query().Get("worker_id")
	token := r.URL.Query().Get("token")
	if workerID == "" || !h.auth(workerID, token) {
		http.Error(w, "unauthorized worker", http.StatusUnauthorized)
		return errors.New("unauthorized worker")
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return err
	}
	conn.SetReadLimit(workerproto.MaxMessageBytes)
	c := &Conn{workerID: workerID, ws: conn, hub: h}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		return nil
	}
	if old := h.conns[workerID]; old != nil {
		_ = old.close()
	}
	h.conns[workerID] = c
	h.online[workerID] = time.Now()
	h.mu.Unlock()
	h.handler.OnConnect(workerID)
	go c.readLoop()
	return nil
}

// Close terminates every worker connection.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	conns := make([]*Conn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		_ = c.close()
	}
}

// Online reports whether a worker has a live connection.
func (h *Hub) Online(workerID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[workerID] != nil
}

// LastSeen returns the last time a worker was heard from (zero if never).
func (h *Hub) LastSeen(workerID string) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.online[workerID]
}

// InFlightCount returns how many jobs a worker is currently running.
func (h *Hub) InFlightCount(workerID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.inFlight[workerID])
}

// SetInFlight replaces a worker's tracked in-flight set (used on hello after a
// reconnect to reconcile ownership).
func (h *Hub) SetInFlight(workerID string, reviewIDs []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := make(map[string]bool, len(reviewIDs))
	for _, id := range reviewIDs {
		if id != "" {
			m[id] = true
		}
	}
	h.inFlight[workerID] = m
}

// AddInFlight marks a job as dispatched to a worker.
func (h *Hub) AddInFlight(workerID, reviewID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.inFlight[workerID]
	if m == nil {
		m = map[string]bool{}
		h.inFlight[workerID] = m
	}
	m[reviewID] = true
}

// ClearInFlight marks a job as finished on a worker.
func (h *Hub) ClearInFlight(workerID, reviewID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := h.inFlight[workerID]; m != nil {
		delete(m, reviewID)
	}
}

// Send pushes a frame to a connected worker.
func (h *Hub) Send(workerID string, frame workerproto.Frame) error {
	h.mu.Lock()
	c := h.conns[workerID]
	h.mu.Unlock()
	if c == nil {
		return fmt.Errorf("worker %s is not connected", workerID)
	}
	return c.send(frame)
}

// Conn is one authenticated worker connection.
type Conn struct {
	workerID string
	ws       *websocket.Conn
	hub      *Hub
	mu       sync.Mutex // serializes writes
}

func (c *Conn) send(frame workerproto.Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return c.ws.Write(ctx, websocket.MessageText, data)
}

func (c *Conn) close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "closing")
}

func (c *Conn) readLoop() {
	defer func() {
		c.hub.mu.Lock()
		current := c.hub.conns[c.workerID] == c
		if current {
			delete(c.hub.conns, c.workerID)
			delete(c.hub.online, c.workerID)
		}
		c.hub.mu.Unlock()
		if current {
			c.hub.handler.OnDisconnect(c.workerID)
		}
	}()
	ctx := context.Background()
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			log.Printf("worker channel closed worker_id=%s status=%d error=%v", c.workerID, websocket.CloseStatus(err), err)
			return
		}
		var frame workerproto.Frame
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		c.hub.mu.Lock()
		c.hub.online[c.workerID] = time.Now()
		c.hub.mu.Unlock()
		if err := c.hub.handler.OnEvent(ctx, c.workerID, frame); err != nil {
			// Handler errors are logged by the handler; keep reading.
		}
	}
}
