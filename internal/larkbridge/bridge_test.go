package larkbridge

import (
	"encoding/json"
	"testing"
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
