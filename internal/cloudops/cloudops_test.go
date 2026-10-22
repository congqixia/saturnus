package cloudops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"saturnus/internal/lark"
)

const samplePayload = `{
  "code": 0,
  "data": {
    "list": [
      {
        "instanceId": "in01-31b61e9752b86b1",
        "jobId": "job-156c6e0a962z1nhuzxq6wv",
        "action": "MODIFY_REPLICA",
        "taskKey": "multi_replica_modify_by_control_center",
        "status": 7,
        "projectCode": "13091281804832",
        "processInstanceId": "5064948",
        "createTime": "2026-09-18 03:50:02",
        "lastUpdate": "2026-09-18 04:01:10",
        "regionId": "aws-us-west-2",
        "params": "{\"sourceReplica\":\"5\",\"replica\":\"9\",\"requestId\":\"ff54db69135db189fbad26bc57ab8274\",\"oldClassId\":\"class-192-5-disk-16cu-enterprise\",\"classId\":\"class-192-9-disk-16cu-enterprise\",\"downgrade\":\"false\"}"
      },
      {
        "instanceId": "in01-31b61e9752b86b1",
        "jobId": "job-15b11a9275dkt9rkp3eomu",
        "action": "MODIFY_CU",
        "taskKey": "multi_replica_modify_by_control_center",
        "status": 3,
        "projectCode": "13091281804832",
        "processInstanceId": "5063841",
        "createTime": "2026-09-18 02:53:41",
        "lastUpdate": "2026-09-18 02:54:31",
        "regionId": "aws-us-west-2",
        "params": "{\"oldClassId\":\"class-208-9-disk-16cu-enterprise\",\"classId\":\"class-192-9-disk-16cu-enterprise\",\"downgrade\":\"true\"}"
      }
    ]
  }
}`

func testClient(t *testing.T) (*Client, *httptest.Server) {
	t.Helper()
	var gotAuth string
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotAuth = r.Header.Get("X-API-Key")
		if !strings.Contains(r.URL.Path, "action/milvus/list") {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("createTimeFrom"); got == "" {
			t.Errorf("missing createTimeFrom query")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(samplePayload))
	}))
	t.Cleanup(func() {
		if called && gotAuth != "test-key" {
			t.Errorf("X-API-Key = %q, want test-key", gotAuth)
		}
	})
	cfg := &Config{
		Environments: []Environment{
			{
				Name:             "prod",
				BaseURL:          srv.URL,
				APIKeyDefault:    "test-key",
				InstancePrefixes: []string{"in01-"},
				PageSize:         200,
				Timeout:          "8s",
			},
		},
		Links: LinkTemplates{
			Dolphin: "{base}/scheduler?id={processInstanceId}&pc={projectCode}",
		},
		Display: DisplayConfig{
			Title: "Cluster Actions",
			Columns: []ColumnConfig{
				{Key: "createTime", Label: "Time", Type: "date", Width: "22%"},
				{Key: "action", Label: "Action", Type: "text", Width: "20%"},
				{Key: "change", Label: "Change", Type: "text", Width: "26%"},
				{Key: "status", Label: "Status", Type: "options", Width: "12%"},
				{Key: "links", Label: "Links", Type: "markdown", Width: "20%"},
			},
		},
		StatusLabels: map[string]string{"3": "failed", "7": "done"},
	}
	t.Cleanup(srv.Close)
	return New(cfg), srv
}

func TestListActions(t *testing.T) {
	client, srv := testClient(t)
	if srv == nil {
		t.Fatal("nil server")
	}
	actions, err := client.ListActions(context.Background(), "in01-31b61e9752b86b1", 24)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(actions))
	}
	a := actions[0]
	if a.Action != "MODIFY_REPLICA" {
		t.Errorf("action = %q", a.Action)
	}
	if a.Status != 7 {
		t.Errorf("status = %d", a.Status)
	}
	if a.Params["sourceReplica"] != "5" || a.Params["replica"] != "9" {
		t.Errorf("params = %v", a.Params)
	}
	if a.CreateTime.IsZero() {
		t.Errorf("createTime not parsed")
	}
}

func TestEnvSelection(t *testing.T) {
	client, _ := testClient(t)
	env := client.envForInstance("in01-abc")
	if env.Name != "prod" {
		t.Errorf("env = %q", env.Name)
	}
	env = client.envForInstance("cn01-abc")
	if env.Name != "prod" {
		t.Errorf("fallback env = %q", env.Name)
	}
}

func TestReply(t *testing.T) {
	client, _ := testClient(t)
	actions, err := client.ListActions(context.Background(), "in01-x", 24)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	card, text := client.Reply("in01-x", 24, actions)
	if card == nil {
		t.Fatalf("expected card, got nil")
	}
	if card.Title == "" {
		t.Errorf("empty card title")
	}
	if len(card.Rows) != 2 {
		t.Errorf("got %d rows, want 2", len(card.Rows))
	}
	row := card.Rows[0]
	if _, ok := row["createTime"].(int64); !ok {
		t.Errorf("createTime cell not int64: %T", row["createTime"])
	}
	if _, ok := row["status"].(map[string]any); !ok {
		t.Errorf("status cell not option tag: %T", row["status"])
	}
	links, ok := row["links"].(string)
	if !ok {
		t.Fatalf("links cell not string: %T", row["links"])
	}
	if !strings.Contains(links, "dolphin") {
		t.Errorf("links = %q", links)
	}
	if !strings.Contains(text, "replica 5→9") {
		t.Errorf("text = %q", text)
	}
	_ = lark.ColumnText
}

func TestReplyEmpty(t *testing.T) {
	client, _ := testClient(t)
	card, text := client.Reply("in01-x", 24, nil)
	if card != nil {
		t.Errorf("expected nil card, got %v", card)
	}
	if !strings.Contains(text, "No actions") {
		t.Errorf("text = %q", text)
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/cloudops.json"
	if err := os.WriteFile(path, []byte(`{
	  "environments": [{"name":"prod","base_url":"https://x.example.com"}],
	  "links": {"dolphin": "{base}/scheduler?id={processInstanceId}"},
	  "display": {"title":"T","columns":[{"key":"action","label":"A","type":"text"}]},
	  "status_labels": {"7":"done"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Environments) != 1 {
		t.Errorf("envs = %d", len(cfg.Environments))
	}
	if got := subst("{base}/scheduler?id={processInstanceId}", map[string]string{"base": "https://x", "processInstanceId": "506"}); got != "https://x/scheduler?id=506" {
		t.Errorf("subst = %q", got)
	}
}

func TestConfigEnvTimeout(t *testing.T) {
	cfg := &Config{Environments: []Environment{{Timeout: "8s"}}}
	client := New(cfg)
	_ = client
	d := cfg.Environments[0].timeout()
	if d != 8*time.Second {
		t.Errorf("timeout = %v", d)
	}
	if cfg.Environments[0].pageSize() != 200 {
		t.Errorf("pageSize = %d", cfg.Environments[0].pageSize())
	}
}
