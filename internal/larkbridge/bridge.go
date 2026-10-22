package larkbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Config struct {
	ServerURL    string
	CLI          string
	EventKey     string
	Identity     string
	ReadyTimeout time.Duration
	PostTimeout  time.Duration
	MaxEvents    string
	Timeout      string
}

func DefaultConfig() Config {
	return Config{
		ServerURL:    env("SATURNUS_SERVER", "http://localhost:8787"),
		CLI:          env("SATURNUS_LARK_CLI", "lark-cli"),
		EventKey:     env("SATURNUS_LARK_EVENT_KEY", "im.message.receive_v1"),
		Identity:     env("SATURNUS_LARK_AS", "bot"),
		ReadyTimeout: envDuration("SATURNUS_LARK_READY_TIMEOUT", 30*time.Second),
		PostTimeout:  envDuration("SATURNUS_LARK_POST_TIMEOUT", 5*time.Minute),
		MaxEvents:    os.Getenv("SATURNUS_LARK_MAX_EVENTS"),
		Timeout:      os.Getenv("SATURNUS_LARK_CONSUME_TIMEOUT"),
	}
}

type Bridge struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) *Bridge {
	cfg.ServerURL = strings.TrimRight(cfg.ServerURL, "/")
	if cfg.PostTimeout <= 0 {
		cfg.PostTimeout = 5 * time.Minute
	}
	return &Bridge{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.PostTimeout},
	}
}

func (b *Bridge) Run(ctx context.Context) error {
	args := []string{"event", "consume", b.cfg.EventKey, "--as", b.cfg.Identity}
	if b.cfg.MaxEvents != "" {
		args = append(args, "--max-events", b.cfg.MaxEvents)
	}
	if b.cfg.Timeout != "" {
		args = append(args, "--timeout", b.cfg.Timeout)
	}
	cmd := exec.CommandContext(ctx, b.cfg.CLI, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	stdin, keepalive := io.Pipe()
	defer keepalive.Close()
	cmd.Stdin = stdin

	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = keepalive.Close()
	}()

	ready := make(chan struct{})
	exited := make(chan struct{})
	go streamStderr(stderr, ready, exited)

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	if err := waitReady(ctx, ready, waitDone, b.cfg.ReadyTimeout); err != nil {
		return err
	}

	consumeDone := make(chan error, 1)
	go func() {
		consumeDone <- b.consume(stdout)
	}()

	// The lark-cli consumer spawns a detached bus daemon that inherits the
	// stdout pipe, so EOF on stdout (or the child process itself) may never
	// arrive on its own after the consumer stops. Bail out as soon as any
	// of the "done" signals fires instead of blocking on the pipe forever.
	var waitErr error
	select {
	case <-consumeDone:
	case <-exited:
	case waitErr = <-waitDone:
	case <-ctx.Done():
	}
	if ctx.Err() != nil {
		return nil
	}
	if waitErr != nil {
		return fmt.Errorf("lark-cli event consumer exited: %w", waitErr)
	}
	return fmt.Errorf("lark-cli event consumer stopped unexpectedly")
}

func (b *Bridge) consume(r io.Reader) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		payload, err := WebhookPayload(line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "saturnus-lark-bridge: skip event: %v\n", err)
			continue
		}
		if err := b.postEvent(payload); err != nil {
			fmt.Fprintf(os.Stderr, "saturnus-lark-bridge: post event failed: %v\n", err)
		}
	}
	return scanner.Err()
}

func WebhookPayload(line []byte) (map[string]any, error) {
	var event map[string]any
	if err := json.Unmarshal(line, &event); err != nil {
		return nil, err
	}
	if stringField(event, "sender_type") == "bot" {
		return nil, fmt.Errorf("bot sender")
	}
	chatID := stringField(event, "chat_id")
	content := stringField(event, "content")
	if chatID == "" {
		return nil, fmt.Errorf("missing chat_id")
	}
	contentJSON, _ := json.Marshal(map[string]string{"text": content})
	return map[string]any{
		"header": map[string]any{
			"event_id":   stringField(event, "event_id"),
			"event_type": stringField(event, "type"),
		},
		"event": map[string]any{
			"sender": map[string]any{
				"sender_id": map[string]any{
					"open_id": stringField(event, "sender_id"),
				},
				"sender_type": stringField(event, "sender_type"),
			},
			"message": map[string]any{
				"chat_id":      chatID,
				"message_id":   firstNonEmpty(stringField(event, "message_id"), stringField(event, "id")),
				"message_type": stringField(event, "message_type"),
				"chat_type":    stringField(event, "chat_type"),
				"reply_to":     stringField(event, "reply_to"),
				"root_id":      stringField(event, "root_id"),
				"thread_id":    stringField(event, "thread_id"),
				"mentions":     mentionItems(event),
				"content":      string(contentJSON),
			},
		},
	}, nil
}

// mentionItems returns the event's mention entries as a JSON-ready slice.
// Each item is {id, key, name}: id is the mentioned user's open_id (or the
// special value "all" for @everyone), so the server can tell "@bot" apart
// from "@all" by comparing ids against the bot's own open_id.
func mentionItems(m map[string]any) []any {
	if raw, ok := m["mentions"].([]any); ok {
		return raw
	}
	return []any{}
}

func (b *Bridge) postEvent(payload map[string]any) error {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, b.cfg.ServerURL+"/lark/events", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}

func streamStderr(r io.Reader, ready chan<- struct{}, exited chan<- struct{}) {
	scanner := bufio.NewScanner(r)
	readySent := false
	exitedSent := false
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(os.Stderr, line)
		if !readySent && strings.Contains(line, "[event] ready event_key=") {
			close(ready)
			readySent = true
		}
		if !exitedSent && strings.Contains(line, "[event] exited ") {
			close(exited)
			exitedSent = true
		}
	}
	if !readySent {
		close(ready)
	}
}

func waitReady(ctx context.Context, ready <-chan struct{}, waitDone <-chan error, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ready:
		return nil
	case err := <-waitDone:
		if err == nil {
			return fmt.Errorf("lark-cli exited before ready")
		}
		return err
	case <-timer.C:
		return fmt.Errorf("timed out waiting for lark-cli event consumer readiness")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func stringField(m map[string]any, key string) string {
	value, _ := m[key].(string)
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
