package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"saturnus/internal/larkbridge"
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
		fatalf("usage: saturnus-agent [flags] register|heartbeat|event|approval|codex-hook|lark-bridge")
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
	case "lark-bridge":
		runLarkBridge()
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
	if sessionID == "" {
		sessionID = firstString(payload, "session_id")
	}
	switch hookType {
	case "SessionStart":
		session, err := registerCodexSession(client, agentID, sessionID, payload)
		if err != nil {
			hookLog("session registration failed: %v", err)
			printJSON(map[string]any{})
			return
		}
		printJSON(map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "SessionStart",
				"additionalContext": "Saturnus is tracking this Codex session as " + session.ID + ".",
			},
		})
	case "PermissionRequest":
		if sessionID == "" {
			hookLog("permission request missing session_id")
			printJSON(map[string]any{})
			return
		}
		if err := ensureCodexSession(client, agentID, sessionID, payload); err != nil {
			hookLog("session ensure failed: %v", err)
		}
		req := map[string]any{
			"session_id": sessionID,
			"tool_name":  firstString(payload, "tool_name", "permission"),
			"tool_input": codexToolInput(payload),
		}
		var out struct {
			Approval map[string]any `json:"approval"`
			Auto     bool           `json:"auto"`
		}
		if err := client.postErr("/api/approvals", req, &out); err != nil {
			hookLog("approval creation failed: %v", err)
			printJSON(map[string]any{})
			return
		}
		status, _ := out.Approval["status"].(string)
		if out.Auto || status == "approved" {
			printCodexPermissionDecision("allow", "")
			return
		}
		requestID, _ := out.Approval["id"].(string)
		decision, reason := waitForCodexDecision(client, requestID)
		switch decision {
		case "approved":
			printCodexPermissionDecision("allow", "")
		case "denied":
			printCodexPermissionDecision("deny", reason)
		default:
			hookLog("approval %s still pending; deferring to native Codex approval", requestID)
			printJSON(map[string]any{})
		}
	default:
		if sessionID == "" {
			printJSON(map[string]any{})
			return
		}
		event := map[string]any{"type": hookType, "payload": payload}
		var out map[string]any
		if err := client.postErr("/api/sessions/"+sessionID+"/events", event, &out); err != nil {
			hookLog("event upload failed: %v", err)
		}
		printJSON(map[string]any{})
	}
}

func runLarkBridge() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	bridge := larkbridge.New(larkbridge.DefaultConfig())
	if err := bridge.Run(ctx); err != nil {
		fatalf("%v", err)
	}
}

func registerCodexSession(client apiClient, agentID string, sessionID string, hook map[string]any) (struct{ ID string }, error) {
	host, _ := os.Hostname()
	if agentID == "" {
		agentID = env("SATURNUS_AGENT_ID", "codex-"+host)
	}
	if sessionID == "" {
		sessionID = firstString(hook, "session_id")
	}
	payload := map[string]any{
		"agent": map[string]any{
			"id":      agentID,
			"name":    env("SATURNUS_AGENT_NAME", "codex"),
			"type":    "codex",
			"host":    host,
			"version": env("SATURNUS_AGENT_VERSION", "codex-hook"),
		},
		"session": map[string]any{
			"id":              sessionID,
			"cwd":             firstString(hook, "cwd"),
			"repo":            env("SATURNUS_REPO", ""),
			"branch":          env("SATURNUS_BRANCH", ""),
			"model":           firstString(hook, "model"),
			"auto_pass":       env("SATURNUS_AUTO_PASS", "") == "1",
			"auto_pass_scope": "codex",
		},
	}
	var out struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := client.postErr("/api/agents/register", payload, &out); err != nil {
		return struct{ ID string }{}, err
	}
	return struct{ ID string }{ID: out.Session.ID}, nil
}

func ensureCodexSession(client apiClient, agentID, sessionID string, hook map[string]any) error {
	var out map[string]any
	if err := client.getErr("/api/sessions/"+sessionID, &out); err == nil {
		return nil
	}
	_, err := registerCodexSession(client, agentID, sessionID, hook)
	return err
}

func codexToolInput(payload map[string]any) any {
	if value, ok := payload["tool_input"]; ok {
		return value
	}
	return payload
}

func waitForCodexDecision(client apiClient, requestID string) (string, string) {
	wait := env("SATURNUS_CODEX_APPROVAL_WAIT", env("SATURNUS_APPROVAL_WAIT", "9m"))
	timeout, err := time.ParseDuration(wait)
	if err != nil {
		timeout = 9 * time.Minute
	}
	if timeout <= 0 {
		return "", ""
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		var got struct {
			Approval map[string]any `json:"approval"`
		}
		if err := client.getErr("/api/approvals/"+requestID, &got); err != nil {
			hookLog("approval poll failed: %v", err)
			continue
		}
		status, _ := got.Approval["status"].(string)
		reason, _ := got.Approval["reason"].(string)
		if status == "approved" || status == "denied" {
			return status, reason
		}
	}
	return "", ""
}

func printCodexPermissionDecision(behavior, message string) {
	decision := map[string]any{"behavior": behavior}
	if message != "" {
		decision["message"] = message
	}
	printJSON(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PermissionRequest",
			"decision":      decision,
		},
	})
}

func (c apiClient) get(path string, out any) {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		fatalf("%v", err)
	}
	if err := c.do(req, out); err != nil {
		fatalf("%v", err)
	}
}

func (c apiClient) post(path string, payload any, out any) {
	if err := c.postErr(path, payload, out); err != nil {
		fatalf("%v", err)
	}
}

func (c apiClient) getErr(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c apiClient) postErr(path string, payload any, out any) error {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c apiClient) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return err
		}
	}
	return nil
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

func hookLog(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "saturnus-agent: "+format+"\n", args...)
}
