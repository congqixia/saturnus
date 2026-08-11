package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type apiClient struct {
	base string
	http *http.Client
}

func main() {
	serverURL := flag.String("server", env("SATURNUS_SERVER", "http://localhost:8787"), "saturnus server URL")
	sessionID := flag.String("session", os.Getenv("SATURNUS_SESSION_ID"), "session ID")
	agentID := flag.String("agent", os.Getenv("SATURNUS_AGENT_ID"), "agent ID")
	flag.Parse()

	client := apiClient{base: strings.TrimRight(*serverURL, "/"), http: &http.Client{Timeout: 30 * time.Second}}
	args := flag.Args()
	if len(args) == 0 {
		fatalf("usage: saturnus-agent [flags] register|heartbeat|event|approval|codex-hook")
	}
	switch args[0] {
	case "register":
		runRegister(client, *agentID)
	case "heartbeat":
		require(*agentID, "agent id")
		runHeartbeat(client, *agentID)
	case "event":
		require(*sessionID, "session id")
		runEvent(client, *sessionID)
	case "approval":
		require(*sessionID, "session id")
		runApproval(client, *sessionID)
	case "codex-hook":
		runCodexHook(client, *agentID, *sessionID)
	default:
		fatalf("unknown command: %s", args[0])
	}
}

func runRegister(client apiClient, agentID string) {
	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	payload := map[string]any{
		"agent": map[string]any{
			"id":      agentID,
			"name":    env("SATURNUS_AGENT_NAME", "codex"),
			"type":    env("SATURNUS_AGENT_TYPE", "codex"),
			"host":    host,
			"version": env("SATURNUS_AGENT_VERSION", "dev"),
		},
		"session": map[string]any{
			"id":              os.Getenv("SATURNUS_SESSION_ID"),
			"cwd":             cwd,
			"repo":            env("SATURNUS_REPO", ""),
			"branch":          env("SATURNUS_BRANCH", ""),
			"model":           env("SATURNUS_MODEL", ""),
			"auto_pass":       env("SATURNUS_AUTO_PASS", "") == "1",
			"auto_pass_scope": "session",
		},
	}
	var out map[string]any
	client.post("/api/agents/register", payload, &out)
	printJSON(out)
}

func runHeartbeat(client apiClient, agentID string) {
	var out map[string]any
	client.post("/api/agents/"+agentID+"/heartbeat", map[string]any{}, &out)
	printJSON(out)
}

func runEvent(client apiClient, sessionID string) {
	payload := readStdinObject()
	if _, ok := payload["type"]; !ok {
		payload["type"] = "event"
	}
	var out map[string]any
	client.post("/api/sessions/"+sessionID+"/events", payload, &out)
	printJSON(out)
}

func runApproval(client apiClient, sessionID string) {
	payload := readStdinObject()
	if _, ok := payload["session_id"]; !ok {
		payload["session_id"] = sessionID
	}
	var out struct {
		Approval map[string]any `json:"approval"`
		Auto     bool           `json:"auto"`
		Reason   string         `json:"reason"`
	}
	client.post("/api/approvals", payload, &out)
	id, _ := out.Approval["id"].(string)
	status, _ := out.Approval["status"].(string)
	if out.Auto || status == "approved" {
		printJSON(map[string]any{"decision": "approved", "request_id": id, "auto": true})
		return
	}
	wait := env("SATURNUS_APPROVAL_WAIT", "10m")
	timeout, err := time.ParseDuration(wait)
	if err != nil {
		timeout = 10 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		var got struct {
			Approval map[string]any `json:"approval"`
		}
		client.get("/api/approvals/"+id, &got)
		status, _ := got.Approval["status"].(string)
		if status == "approved" || status == "denied" {
			printJSON(map[string]any{"decision": status, "request_id": id, "auto": false})
			if status == "denied" {
				os.Exit(2)
			}
			return
		}
	}
	printJSON(map[string]any{"decision": "timeout", "request_id": id})
	os.Exit(3)
}

func runCodexHook(client apiClient, agentID, sessionID string) {
	payload := readStdinObject()
	hookType := firstString(payload, "hook_event_name", "type", "event")
	switch hookType {
	case "SessionStart":
		runRegister(client, agentID)
	case "PermissionRequest":
		if sessionID == "" {
			sessionID = firstString(payload, "session_id")
		}
		require(sessionID, "session id")
		req := map[string]any{
			"session_id": sessionID,
			"tool_name":  firstString(payload, "tool_name", "command", "permission"),
			"tool_input": payload,
		}
		var out struct {
			Approval map[string]any `json:"approval"`
			Auto     bool           `json:"auto"`
		}
		client.post("/api/approvals", req, &out)
		status, _ := out.Approval["status"].(string)
		if out.Auto || status == "approved" {
			printJSON(map[string]any{"decision": "allow"})
			return
		}
		printJSON(map[string]any{"decision": "ask", "request_id": out.Approval["id"]})
	default:
		if sessionID == "" {
			sessionID = firstString(payload, "session_id")
		}
		if sessionID == "" {
			printJSON(map[string]any{"ignored": true, "reason": "missing session id"})
			return
		}
		event := map[string]any{"type": hookType, "payload": payload}
		var out map[string]any
		client.post("/api/sessions/"+sessionID+"/events", event, &out)
		printJSON(out)
	}
}

func (c apiClient) get(path string, out any) {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		fatalf("%v", err)
	}
	c.do(req, out)
}

func (c apiClient) post(path string, payload any, out any) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		fatalf("%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.do(req, out)
}

func (c apiClient) do(req *http.Request, out any) {
	resp, err := c.http.Do(req)
	if err != nil {
		fatalf("%v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		fatalf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			fatalf("%v", err)
		}
	}
}

func readStdinObject() map[string]any {
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		fatalf("%v", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return map[string]any{}
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		fatalf("invalid json stdin: %v", err)
	}
	return payload
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func require(value, name string) {
	if value == "" {
		fatalf("missing %s", name)
	}
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
