package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"saturnus/internal/prs"
	"saturnus/internal/review"
	"saturnus/internal/store"
)

func testPRServer(t *testing.T) *server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "saturnus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cli := filepath.Join(t.TempDir(), "gh")
	script := `#!/bin/sh
case "$4" in
user) echo '{"login":"me"}' ;;
repos/a/b/pulls\?*) echo '[[{"number":1,"user":{"login":"me"}}]]' ;;
repos/a/b/pulls/1) echo '{"number":1,"state":"open","title":"CI example","head":{"sha":"head","ref":"feature"},"base":{"ref":"main"},"user":{"login":"me"},"html_url":"https://github.com/a/b/pull/1"}' ;;
*/actions/runs\?*) echo '[{"total_count":0,"workflow_runs":[]}]' ;;
*/check-runs\?*) echo '[{"total_count":1,"check_runs":[{"id":1,"name":"build","status":"completed","conclusion":"success","app":{"id":1,"slug":"github-actions"}}]}]' ;;
*/statuses\?*) echo '[[{"id":2,"context":"optional report","state":"failure","target_url":"https://ci.example/2"}]]' ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(cli, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return &server{store: st, prs: prs.New(st, prs.Config{Enabled: true, Repos: []string{"a/b"}, GHCli: cli, Policy: prs.Policy{Repos: map[string][]string{"a/b": {"optional report"}}}})}
}

func TestPRCommandsAndAPIPreserveBypassedFailures(t *testing.T) {
	s := testPRServer(t)
	for _, command := range []string{"/sat pr a/b#1", "/sat describe pr https://github.com/a/b/pull/1"} {
		reply := s.handleBotCommand(command, review.MessageContext{})
		if reply.md == nil {
			t.Fatalf("%s: %s", command, reply.text)
		}
		if reply.table == nil || len(reply.table.Rows) != 2 || !strings.Contains(reply.table.Description, "**CI**: PASS") {
			t.Fatalf("missing native CI table: %+v", reply.table)
		}
		if row := reply.table.Rows[1]; row["bypass"] != "YES (repo:a/b)" || row["state"].(map[string]any)["text"] != "completed / failure" {
			t.Fatalf("bypassed failure not visible: %v", row)
		}
		for _, want := range []string{"**CI**: PASS", "optional report", "completed / failure", "YES (repo:a/b)", "https://ci.example/2"} {
			if !strings.Contains(reply.md.Content, want) {
				t.Fatalf("%s missing %q: %s", command, want, reply.md.Content)
			}
		}
	}
	listed := s.prsReply()
	if listed.table == nil || len(listed.table.Rows) != 1 || listed.table.Rows[0]["bypass"] != 1 {
		t.Fatalf("CI summary missing: %+v", listed)
	}
	if tag := listed.table.Rows[0]["ci"].(map[string]any); tag["text"] != "PASS" {
		t.Fatal(tag)
	}
	rr := httptest.NewRecorder()
	s.listPRs(rr, httptest.NewRequest("GET", "/api/prs", nil))
	var listing struct {
		PRs []store.PullRequest `json:"prs"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listing); err != nil || len(listing.PRs) != 1 {
		t.Fatalf("%s %v", rr.Body.String(), err)
	}
	id := listing.PRs[0].ID
	request := httptest.NewRequest("GET", "/api/prs/"+id, nil)
	request.SetPathValue("pr_id", id)
	rr = httptest.NewRecorder()
	s.getPR(rr, request)
	var d prs.Detail
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil || rr.Code != 200 || d.PR.CIState != "PASS" || len(d.Checks) != 2 {
		t.Fatalf("%s %v", rr.Body.String(), err)
	}
	if !d.Checks[1].Bypassed || d.Checks[1].Bucket != "fail" {
		t.Fatalf("bypassed failure hidden: %+v", d.Checks)
	}
	request = httptest.NewRequest(http.MethodPatch, "/api/prs/"+id, strings.NewReader(`{"monitoring":false}`))
	request.SetPathValue("pr_id", id)
	rr = httptest.NewRecorder()
	s.updatePRMonitoring(rr, request)
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil || rr.Code != 200 || d.PR.Monitoring {
		t.Fatalf("pause failed: %s %v", rr.Body.String(), err)
	}
	if reply := s.handleBotCommand("/sat pr resume a/b#1", review.MessageContext{}); !strings.Contains(reply.text, "monitoring=true") {
		t.Fatal(reply.text)
	}
	request = httptest.NewRequest("POST", "/api/prs", strings.NewReader(`{"url":"outside/repo#1"}`))
	rr = httptest.NewRecorder()
	s.trackPR(rr, request)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("whitelist error: %d %s", rr.Code, rr.Body.String())
	}
}

func TestPRBypassConfigValidation(t *testing.T) {
	t.Setenv("SATURNUS_PRS_REFRESH_INTERVAL", "0")
	t.Setenv("SATURNUS_PRS_REFRESH_TIMEOUT", "30s")
	file := filepath.Join(t.TempDir(), "bypass.json")
	if err := os.WriteFile(file, []byte(`{"global":["optional"],"repos":{"a/b":["report"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SATURNUS_PRS_BYPASS_FILE", file)
	cfg, err := prsConfig(map[string]string{"a/b": "/unused-checkout"})
	if err != nil || cfg.RefreshInterval != 0 || cfg.Policy.Rule("a/b", "report") != "repo:a/b" {
		t.Fatalf("%+v %v", cfg, err)
	}
	if err := os.WriteFile(file, []byte(`{"unknown":["optional"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := prsConfig(nil); err == nil {
		t.Fatal("invalid bypass file accepted")
	}
}
