package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLinkCodexTaskOnStartAndResume(t *testing.T) {
	linked := false
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tasks/tsk_test" && r.Method == http.MethodGet {
			if linked {
				_, _ = w.Write([]byte(`{"sessions":[{"tool":"codex","session_id":"session-1"}]}`))
			} else {
				_, _ = w.Write([]byte(`{"sessions":[]}`))
			}
			return
		}
		if r.URL.Path == "/api/tasks/tsk_test/sessions" && r.Method == http.MethodPost {
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input["tool"] != "codex" || input["session_id"] != "session-1" || input["purpose"] != "coding" {
				t.Errorf("bad input: %v", input)
			}
			posts++
			linked = true
			w.WriteHeader(http.StatusCreated)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := apiClient{base: server.URL, http: server.Client()}
	for i := 0; i < 2; i++ {
		if err := linkCodexTask(client, "tsk_test", "session-1", "coding"); err != nil {
			t.Fatal(err)
		}
	}
	if posts != 1 {
		t.Fatalf("expected one link, got %d", posts)
	}
	if err := linkCodexTask(client, "missing", "session-1", ""); err == nil {
		t.Fatal("expected missing task error")
	}
}
