package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"saturnus/internal/ghinfo"
	"saturnus/internal/store"
)

func TestFileReposRegisteredAsWorkerCapabilities(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "repos.json")
	if err := os.WriteFile(path, []byte(`{"owner/repo":"checkout,with=punctuation"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	repos, err := loadRepos("", path)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan registerRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/workers/register" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req registerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- req
		_ = json.NewEncoder(w).Encode(store.Agent{ID: "worker", WorkerToken: "token"})
	}))
	defer srv.Close()
	expiry := time.Now().UTC().Add(6 * 24 * time.Hour)
	info := &ghinfo.Info{Version: "2.86.0", Host: ghinfo.Host, TokenExpiresAt: &expiry, ExpiryStatus: "known"}
	agent, err := register(srv.URL, "worker", "key", "opencode", repos, true, 2, info)
	if err != nil || agent.WorkerToken != "token" {
		t.Fatalf("agent=%+v err=%v", agent, err)
	}
	select {
	case req := <-requests:
		if req.Capabilities.GitHub == nil || req.Capabilities.GitHub.Version != "2.86.0" || !req.Capabilities.GitHub.TokenExpiresAt.Equal(expiry) {
			t.Fatalf("GitHub metadata missing from registration: %+v", req.Capabilities)
		}
		want := map[string]string{"owner/repo": filepath.Join(base, "checkout,with=punctuation")}
		if !reflect.DeepEqual(req.Capabilities.Repos, want) {
			t.Fatalf("registered repos=%v, want %v", req.Capabilities.Repos, want)
		}
	default:
		t.Fatal("worker did not register")
	}
}

func TestLoadReposRequiresNonEmptyMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repos.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ inline, file string }{{"", ""}, {"bad", ""}, {"", path}} {
		if _, err := loadRepos(tc.inline, tc.file); err == nil || !strings.Contains(err.Error(), "no repos configured") {
			t.Fatalf("loadRepos(%q, %q) err=%v", tc.inline, tc.file, err)
		}
	}
	if _, err := loadRepos("owner/repo=/checkout", path); err == nil || !strings.Contains(err.Error(), "sources conflict") {
		t.Fatalf("accepted conflicting worker repo sources: %v", err)
	}
}
