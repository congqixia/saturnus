package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"saturnus/internal/repoconfig"
	"saturnus/internal/reviewworker"
	"saturnus/internal/store"
)

// registerRequest is the payload POSTed to /api/workers/register.
type registerRequest struct {
	Agent        store.Agent              `json:"agent"`
	Capabilities store.WorkerCapabilities `json:"capabilities"`
	RegisterKey  string                   `json:"register_key"`
}

func main() {
	serverURL := flag.String("server", env("SATURNUS_SERVER", "http://localhost:8787"), "saturnus server URL")
	workerID := flag.String("worker", env("SATURNUS_WORKER_ID", ""), "worker id (defaults to a hostname-derived id)")
	registerKey := flag.String("register-key", env("SATURNUS_WORKER_REGISTER_KEY", ""), "shared registration key")
	tool := flag.String("tool", env("SATURNUS_REVIEW_TOOL", "opencode"), "review tool executable")
	reposRaw := flag.String("repos", os.Getenv("SATURNUS_REVIEW_REPOS"), "owner/repo=/path/to/checkout,...")
	reposFile := flag.String("repos-file", os.Getenv("SATURNUS_REVIEW_REPOS_FILE"), "JSON file mapping owner/repo to local checkout path (mutually exclusive with -repos)")
	gh := flag.Bool("gh", envBool("SATURNUS_REVIEW_GH", true), "worker has gh available")
	maxConcurrent := flag.Int("max-concurrent", envInt("SATURNUS_WORKER_MAX_CONCURRENT", 1), "concurrent review jobs")
	logDir := flag.String("log-dir", os.Getenv("SATURNUS_REVIEW_LOG_DIR"), "job transcript log directory")
	flag.Parse()

	repos, err := loadRepos(*reposRaw, *reposFile)
	if err != nil {
		log.Fatal(err)
	}
	if *registerKey == "" {
		log.Fatal("missing registration key: set SATURNUS_WORKER_REGISTER_KEY or -register-key")
	}
	host, _ := os.Hostname()
	if *workerID == "" {
		*workerID = "wrk-" + sanitize(host)
	}

	// Register with the central server and obtain the control-channel token.
	agent, err := register(*serverURL, *workerID, *registerKey, *tool, repos, *gh, *maxConcurrent)
	if err != nil {
		log.Fatalf("register with server: %v", err)
	}
	if agent.WorkerToken == "" {
		log.Fatal("server did not issue a worker token")
	}

	w := reviewworker.New(reviewworker.Config{
		ServerURL:     strings.TrimRight(*serverURL, "/"),
		WorkerID:      agent.ID,
		Token:         agent.WorkerToken,
		Tool:          *tool,
		Repos:         repos,
		GH:            *gh,
		MaxConcurrent: *maxConcurrent,
		LogDir:        *logDir,
	})
	defer w.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("saturnus-worker %s registered with %s (host=%s, tool=%s, repos=%d, max_concurrent=%d)",
		agent.ID, *serverURL, host, *tool, len(repos), *maxConcurrent)
	if err := w.Run(ctx); err != nil {
		log.Fatalf("%v", err)
	}
}

func loadRepos(inline, file string) (map[string]string, error) {
	repos, err := repoconfig.Load(inline, file)
	if err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("no repos configured: set SATURNUS_REVIEW_REPOS_FILE (-repos-file) or SATURNUS_REVIEW_REPOS (-repos)")
	}
	return repos, nil
}

// register POSTs the worker's identity and capabilities and returns the agent
// with its issued worker token.
func register(serverURL, workerID, registerKey, tool string, repos map[string]string, gh bool, maxConcurrent int) (store.Agent, error) {
	host, _ := os.Hostname()
	req := registerRequest{
		Agent: store.Agent{
			ID:      workerID,
			Name:    "review-worker",
			Type:    "review-worker",
			Host:    host,
			Version: env("SATURNUS_WORKER_VERSION", "dev"),
		},
		Capabilities: store.WorkerCapabilities{
			Repos:         repos,
			Tool:          tool,
			ToolVersion:   env("SATURNUS_REVIEW_TOOL_VERSION", ""),
			GH:            gh,
			MaxConcurrent: maxConcurrent,
		},
		RegisterKey: registerKey,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return store.Agent{}, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, strings.TrimRight(serverURL, "/")+"/api/workers/register", bytes.NewReader(body))
	if err != nil {
		return store.Agent{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return store.Agent{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return store.Agent{}, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var out store.Agent
	if err := json.Unmarshal(data, &out); err != nil {
		return store.Agent{}, err
	}
	return out, nil
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "worker"
	}
	return b.String()
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
