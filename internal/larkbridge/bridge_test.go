package larkbridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWebhookPayloadWrapsLarkCLIEvent(t *testing.T) {
	payload, err := WebhookPayload([]byte(`{
		"type":"im.message.receive_v1",
		"event_id":"evt_1",
		"chat_id":"oc_123",
		"message_id":"om_123",
		"message_type":"text",
		"content":"/sat pending",
		"sender_id":"ou_123",
		"sender_type":"user"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	event := payload["event"].(map[string]any)
	message := event["message"].(map[string]any)
	if message["chat_id"] != "oc_123" {
		t.Fatalf("chat_id = %q, want oc_123", message["chat_id"])
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(message["content"].(string)), &content); err != nil {
		t.Fatal(err)
	}
	if content.Text != "/sat pending" {
		t.Fatalf("content text = %q, want /sat pending", content.Text)
	}
}

func TestWebhookPayloadRejectsMissingChatID(t *testing.T) {
	if _, err := WebhookPayload([]byte(`{"content":"/sat pending"}`)); err == nil {
		t.Fatal("expected missing chat_id error")
	}
}

func TestWebhookPayloadRejectsBotSender(t *testing.T) {
	if _, err := WebhookPayload([]byte(`{
		"chat_id":"oc_123",
		"content":"Commands: /sat sessions",
		"sender_type":"bot"
	}`)); err == nil {
		t.Fatal("expected bot sender error")
	}
}

func TestWebhookPayloadForwardsChatTypeAndMentions(t *testing.T) {
	payload, err := WebhookPayload([]byte(`{
		"type":"im.message.receive_v1",
		"chat_type":"group",
		"chat_id":"oc_123",
		"message_id":"om_123",
		"message_type":"text",
		"content":"hi",
		"sender_id":"ou_user",
		"sender_type":"user",
		"mentions":[
			{"id":"all","key":"@_user_1","name":"所有人"},
			{"id":"ou_bot","key":"@_user_2","name":"saturnus"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	event := payload["event"].(map[string]any)
	message := event["message"].(map[string]any)
	if message["chat_type"] != "group" {
		t.Fatalf("chat_type = %v, want group", message["chat_type"])
	}
	mentions, ok := message["mentions"].([]any)
	if !ok {
		t.Fatalf("mentions not forwarded: %#v", message["mentions"])
	}
	if len(mentions) != 2 {
		t.Fatalf("mentions len = %d, want 2", len(mentions))
	}
}

func TestWebhookPayloadDefaultsEmptyMentions(t *testing.T) {
	payload, err := WebhookPayload([]byte(`{
		"type":"im.message.receive_v1",
		"chat_type":"p2p",
		"chat_id":"oc_123",
		"message_id":"om_123",
		"message_type":"text",
		"content":"hi",
		"sender_id":"ou_user",
		"sender_type":"user"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	event := payload["event"].(map[string]any)
	message := event["message"].(map[string]any)
	mentions, ok := message["mentions"].([]any)
	if !ok || len(mentions) != 0 {
		t.Fatalf("mentions = %#v, want empty slice", message["mentions"])
	}
}

func TestWebhookPayloadForwardsReplyContext(t *testing.T) {
	payload, err := WebhookPayload([]byte(`{
		"type":"im.message.receive_v1",
		"chat_type":"group",
		"chat_id":"oc_123",
		"message_id":"om_reply",
		"reply_to":"om_parent",
		"root_id":"om_root",
		"thread_id":"omt_root",
		"message_type":"text",
		"content":"/sat sessions",
		"sender_id":"ou_user",
		"sender_type":"user"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	event := payload["event"].(map[string]any)
	message := event["message"].(map[string]any)
	if message["reply_to"] != "om_parent" {
		t.Fatalf("reply_to = %v, want om_parent", message["reply_to"])
	}
	if message["root_id"] != "om_root" {
		t.Fatalf("root_id = %v, want om_root", message["root_id"])
	}
	if message["thread_id"] != "omt_root" {
		t.Fatalf("thread_id = %v, want omt_root", message["thread_id"])
	}
}

func TestBridgeExitsWhenConsumerAnnouncesExit(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "fake-cli")
	script := "#!/bin/sh\n" +
		"echo '[event] ready event_key=im.message.receive_v1' >&2\n" +
		"echo '[event] exited — received 0 event(s) in 0s (reason: signal)' >&2\n" +
		"cat >/dev/null\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ServerURL:    "http://127.0.0.1:1", // nothing is posted: no stdout output
		CLI:          cli,
		EventKey:     "im.message.receive_v1",
		Identity:     "bot",
		ReadyTimeout: 5 * time.Second,
	}
	start := time.Now()
	err := New(cfg).Run(context.Background())
	if err == nil {
		t.Fatal("expected error when consumer stops unexpectedly")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Run blocked %s after exit marker; pipe not released", elapsed)
	}
}

func TestBridgeExitsWhenConsumerProcessDies(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "fake-cli")
	script := "#!/bin/sh\n" +
		"echo '[event] ready event_key=im.message.receive_v1' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ServerURL:    "http://127.0.0.1:1",
		CLI:          cli,
		EventKey:     "im.message.receive_v1",
		Identity:     "bot",
		ReadyTimeout: 5 * time.Second,
	}
	start := time.Now()
	err := New(cfg).Run(context.Background())
	if err == nil {
		t.Fatal("expected error when consumer process exits")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Run blocked %s after child exit; pipe not released", elapsed)
	}
}
