package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"saturnus/internal/workerproto"
)

type transportHandler struct {
	connected chan struct{}
	events    chan workerproto.Frame
	once      sync.Once
}

func (h *transportHandler) OnConnect(string)    { h.once.Do(func() { close(h.connected) }) }
func (h *transportHandler) OnDisconnect(string) {}
func (h *transportHandler) OnEvent(_ context.Context, _ string, f workerproto.Frame) error {
	h.events <- f
	return nil
}

func TestLargeControlFramesInBothDirections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := &transportHandler{connected: make(chan struct{}), events: make(chan workerproto.Frame, 8)}
	hub := NewHub(h, func(string, string) bool { return true })
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = hub.ServeWS(w, r) }))
	defer ts.Close()
	defer hub.Close()
	commands := make(chan workerproto.Frame, 1)
	client := NewClient(ClientConfig{
		ServerURL: ts.URL, WorkerID: "worker", Token: "test", InFlight: func() []string { return nil },
		OnCommand: func(_ context.Context, f workerproto.Frame) error { commands <- f; return nil },
	})
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	defer func() {
		cancel()
		client.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("client did not stop")
		}
	}()
	select {
	case <-h.connected:
	case <-ctx.Done():
		t.Fatal("worker did not connect")
	}
	// JSON escaping can expand the 512 KiB transcript well beyond its raw
	// size. Exercise that boundary, as well as claims larger than 32 KiB.
	text := strings.Repeat("\x01", 512*1024)
	if err := hub.Send("worker", workerproto.NewFrame(workerproto.CmdClaim, "review", workerproto.JobPayload{Guide: text})); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-commands:
		var p workerproto.JobPayload
		if err := f.Decode(&p); err != nil || p.Guide != text {
			t.Fatalf("claim truncated: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("large claim disconnected the worker")
	}
	if err := client.Send(workerproto.NewFrame(workerproto.EvComplete, "review", workerproto.CompletePayload{OK: true, ResultText: text})); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case f := <-h.events:
			if f.Type != workerproto.EvComplete {
				continue
			}
			var p workerproto.CompletePayload
			if err := f.Decode(&p); err != nil || p.ResultText != text {
				t.Fatalf("completion truncated: %v", err)
			}
			return
		case <-ctx.Done():
			t.Fatal("large completion disconnected the worker")
		}
	}
}
