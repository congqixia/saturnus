package lark

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseContactUserName(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "name", raw: `{"code":0,"data":{"user":{"name":"张三"}}}`, want: "张三"},
		{name: "en name fallback", raw: `{"code":0,"data":{"user":{"en_name":"Zhang San"}}}`, want: "Zhang San"},
		{name: "nickname fallback", raw: `{"code":0,"data":{"user":{"nickname":"zs"}}}`, want: "zs"},
		{name: "empty", raw: `{"code":0,"data":{"user":{}}}`, want: ""},
		{name: "error code", raw: `{"code":99991672,"msg":"no permission"}`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseContactUserName([]byte(tc.raw))
			if tc.want == "" && tc.name == "error code" {
				if err == nil {
					t.Fatal("expected error for non-zero code")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseContactUsers(t *testing.T) {
	raw := []byte(`{
		"code": 0,
		"msg": "ok",
		"data": {
			"user_list": [
				{"user_id": "ou_email_user", "email": "a@example.com", "mobile": "13800000001"},
				{"open_id": "ou_name_user", "name": "Zhang San", "email": "zs@example.com"},
				{"user_id": "ou_no_contact"}
			]
		}
	}`)
	users, err := parseContactUsers(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 3 {
		t.Fatalf("got %d users, want 3", len(users))
	}
	if users[0].OpenID != "ou_email_user" {
		t.Fatalf("expected user_id fallback as open_id, got %q", users[0].OpenID)
	}
	if users[1].OpenID != "ou_name_user" || users[1].Name != "Zhang San" {
		t.Fatalf("unexpected search result: %#v", users[1])
	}
}

func TestParseContactUsersError(t *testing.T) {
	raw := []byte(`{"code": 99991672, "msg": "no permission"}`)
	if _, err := parseContactUsers(raw); err == nil {
		t.Fatal("expected error for non-zero code")
	}
}

func TestIsAllDigits(t *testing.T) {
	if !isAllDigits("13800000001") {
		t.Fatal("expected true for numeric mobile")
	}
	if isAllDigits("138-0000") {
		t.Fatal("expected false for mixed value")
	}
	if isAllDigits("") {
		t.Fatal("expected false for empty")
	}
}

func TestLarkErrorDetail(t *testing.T) {
	got := larkErrorDetail([]byte(`{"code":1470400,"msg":"invalid params"}`))
	if !strings.Contains(got, "1470400") || !strings.Contains(got, "invalid params") {
		t.Fatalf("expected code+msg in error detail, got %q", got)
	}
	// Non-JSON bodies fall back to a quoted snippet.
	got = larkErrorDetail([]byte("<html>gateway error</html>"))
	if !strings.Contains(got, "gateway error") {
		t.Fatalf("expected body fallback, got %q", got)
	}
	if got := larkErrorDetail(nil); got != "" {
		t.Fatalf("expected empty detail for empty body, got %q", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Fatalf("expected no truncation, got %q", got)
	}
	got := truncateRunes("你好世界", 3)
	if got != "你好世" || len([]rune(got)) != 3 {
		t.Fatalf("expected rune-safe truncation, got %q", got)
	}
}

// TestUpdateTaskBody verifies the task v2 PATCH contract: a `task` object plus
// `update_fields`, and completion expressed via `completed_at` (ms timestamp).
func TestUpdateTaskBody(t *testing.T) {
	desc := strings.Repeat("长", taskDescriptionMaxRunes+100)
	completed := true
	payload, ok := buildTaskUpdate(UpdateTaskInput{Description: &desc, Completed: &completed})
	if !ok {
		t.Fatal("expected a payload to build")
	}
	raw, _ := json.Marshal(payload)
	if !strings.Contains(string(raw), `"update_fields"`) || !strings.Contains(string(raw), `"completed_at"`) {
		t.Fatalf("update body missing update_fields/completed_at: %s", raw)
	}
	if strings.Contains(string(raw), `"completed"`) {
		t.Fatalf("update body must not use a top-level completed field: %s", raw)
	}
	task := payload["task"].(map[string]any)
	if len([]rune(task["description"].(string))) > taskDescriptionMaxRunes {
		t.Fatalf("description not truncated to %d runes", taskDescriptionMaxRunes)
	}

	// Reopen: completed=false maps to completed_at "0".
	reopen := false
	payload2, _ := buildTaskUpdate(UpdateTaskInput{Completed: &reopen})
	if payload2["task"].(map[string]any)["completed_at"] != "0" {
		t.Fatalf("expected completed_at 0 for reopen, got %#v", payload2)
	}

	// No fields to update -> not ok.
	if _, ok := buildTaskUpdate(UpdateTaskInput{}); ok {
		t.Fatal("expected no payload for empty input")
	}
}
