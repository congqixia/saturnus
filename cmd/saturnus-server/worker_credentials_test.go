package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"saturnus/internal/ghinfo"
	"saturnus/internal/store"
	"saturnus/internal/workerproto"
)

func TestWorkerGitHubRegistrationAndHeartbeat(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	expiry := now.Add(6 * 24 * time.Hour)
	info := &ghinfo.Info{Version: "2.86.0", Host: ghinfo.Host, CheckedAt: now, TokenExpiresAt: &expiry, ExpiryStatus: "known"}
	body, _ := json.Marshal(map[string]any{
		"agent": store.Agent{ID: "worker"}, "register_key": "key",
		"capabilities": store.WorkerCapabilities{GH: true, GitHub: info, Repos: map[string]string{"a/b": "/checkout"}},
	})
	srv := &server{store: st, workerRegisterKey: "key"}
	rr := httptest.NewRecorder()
	srv.registerWorker(rr, httptest.NewRequest("POST", "/api/workers/register", bytes.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("register: %d %s", rr.Code, rr.Body.String())
	}
	agent, err := st.GetWorker("worker")
	if err != nil {
		t.Fatal(err)
	}
	caps, err := agent.CapabilitiesDecoded()
	if err != nil || caps.GitHub == nil || caps.GitHub.Version != "2.86.0" || !caps.GitHub.TokenExpiresAt.Equal(expiry) {
		t.Fatalf("caps=%+v err=%v", caps, err)
	}
	notified := 0
	sched := newScheduler(nil, st)
	sched.onWorkerHeartbeat = func() { notified++ }
	updated := *info
	updated.CheckedAt = now.Add(time.Hour)
	updated.Version = "2.87.0"
	newExpiry := now.Add(30 * 24 * time.Hour)
	updated.TokenExpiresAt = &newExpiry
	frame := workerproto.NewFrame(workerproto.EvHeartbeat, "", workerproto.HeartbeatPayload{GitHub: &updated})
	if err := sched.OnEvent(context.Background(), "worker", frame); err != nil {
		t.Fatal(err)
	}
	agent, _ = st.GetWorker("worker")
	caps, _ = agent.CapabilitiesDecoded()
	if caps.GitHub.Version != "2.87.0" || !caps.GitHub.TokenExpiresAt.Equal(newExpiry) || caps.Repos["a/b"] != "/checkout" || notified != 1 {
		t.Fatalf("update lost: %+v notified=%d", caps, notified)
	}
	// Repeated/stale heartbeats cannot undo token rotation or trigger rescans.
	for _, snapshot := range []*ghinfo.Info{&updated, info} {
		if err := sched.OnEvent(context.Background(), "worker", workerproto.NewFrame(workerproto.EvHeartbeat, "", workerproto.HeartbeatPayload{GitHub: snapshot})); err != nil {
			t.Fatal(err)
		}
	}
	agent, _ = st.GetWorker("worker")
	caps, _ = agent.CapabilitiesDecoded()
	if notified != 1 || !caps.GitHub.TokenExpiresAt.Equal(newExpiry) {
		t.Fatal("stale heartbeat replaced token metadata")
	}
	// Old workers' heartbeat payloads are still accepted.
	if err := sched.OnEvent(context.Background(), "worker", workerproto.NewFrame(workerproto.EvHeartbeat, "", workerproto.HeartbeatPayload{})); err != nil {
		t.Fatal(err)
	}
}
