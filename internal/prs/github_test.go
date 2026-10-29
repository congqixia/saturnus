package prs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func shellLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func githubStub(t *testing.T, responses map[string]string) GitHub {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("#!/bin/sh\nif [ \"$1 $2 $3\" != 'api --method GET' ]; then exit 2; fi\ncase \"$4\" in\n")
	i := 0
	for endpoint, response := range responses {
		file := filepath.Join(dir, string(rune('a'+i))+".json")
		i++
		if err := os.WriteFile(file, []byte(response), 0600); err != nil {
			t.Fatal(err)
		}
		b.WriteString(shellLiteral(endpoint) + ") cat " + shellLiteral(file) + " ;;\n")
	}
	b.WriteString("*) exit 1 ;;\nesac\n")
	cli := filepath.Join(dir, "gh")
	if err := os.WriteFile(cli, []byte(b.String()), 0755); err != nil {
		t.Fatal(err)
	}
	return GitHub{CLI: cli}
}

const noWorkflowRuns = `[{"total_count":0,"workflow_runs":[]}]`

func prJSON(head, merge string) string {
	return `{"number":1,"state":"open","title":"PR","user":{"login":"me"},"head":{"sha":"` + head + `","ref":"feature","repo":{"full_name":"fork/repo"}},"base":{"ref":"main"},"merge_commit_sha":"` + merge + `"}`
}

func TestGitHubPaginatesAndUsesLatestChecksAndTestMerge(t *testing.T) {
	g := githubStub(t, map[string]string{
		"user": `{"login":"me"}`,
		"repos/a/b/pulls?state=open&per_page=100":                    `[[{"number":1,"user":{"login":"me"}},{"number":2,"user":{"login":"other"}}],[{"number":3,"user":{"login":"me"}}]]`,
		"repos/a/b/pulls/1":                                          prJSON("head", "merge"),
		"repos/a/b/actions/runs?head_sha=head&per_page=100":          noWorkflowRuns,
		"repos/a/b/actions/runs?head_sha=merge&per_page=100":         noWorkflowRuns,
		"repos/a/b/commits/head/check-runs?filter=all&per_page=100":  `[{"total_count":2,"check_runs":[{"id":301,"name":"build","head_sha":"head","status":"completed","conclusion":"failure","app":{"id":1,"slug":"github-actions"}}]},{"total_count":2,"check_runs":[{"id":300,"name":"build","head_sha":"head","status":"completed","conclusion":"success","app":{"id":1,"slug":"github-actions"}}]}]`,
		"repos/a/b/commits/head/statuses?per_page=100":               `[[{"id":50,"context":"optional","state":"success"}],[{"id":49,"context":"optional","state":"failure"}]]`,
		"repos/a/b/commits/merge/check-runs?filter=all&per_page=100": `[{"total_count":1,"check_runs":[{"id":250,"name":"build","head_sha":"merge","status":"completed","conclusion":"success","app":{"id":1,"slug":"github-actions"}}]}]`,
		"repos/a/b/commits/merge/statuses?per_page=100":              `[[{"id":40,"context":"optional","state":"pending","target_url":"https://ci.example/build"}]]`,
	})
	ctx := context.Background()
	user, err := g.CurrentUser(ctx)
	if err != nil || user != "me" {
		t.Fatalf("%s %v", user, err)
	}
	prs, err := g.Discover(ctx, "a/b", user)
	if err != nil || len(prs) != 2 || prs[1] != 3 {
		t.Fatalf("%v %v", prs, err)
	}
	pr, checks, err := g.Fetch(ctx, "a/b", 1)
	if err != nil || pr.HeadRepo != "fork/repo" || len(checks) != 2 {
		t.Fatalf("pr=%+v checks=%+v err=%v", pr, checks, err)
	}
	for _, c := range checks {
		if c.HeadSHA != "merge" {
			t.Fatal(c)
		}
		switch c.Name {
		case "build":
			if c.Bucket != "pass" || c.ExternalID != 250 {
				t.Fatal(c)
			}
		case "optional":
			if c.Bucket != "pending" || c.ExternalID != 40 {
				t.Fatal(c)
			}
		}
	}
}

func TestGitHubMalformedOrPartialSourcesNeverBecomeNoChecks(t *testing.T) {
	for _, payload := range []string{`null`, `[]`, `[{}]`, `[{"total_count":0,"check_runs":null}]`, `not json`} {
		g := githubStub(t, map[string]string{"repos/a/b/pulls/1": prJSON("head", ""), "repos/a/b/actions/runs?head_sha=head&per_page=100": noWorkflowRuns, "repos/a/b/commits/head/check-runs?filter=all&per_page=100": payload, "repos/a/b/commits/head/statuses?per_page=100": `[[]]`})
		if _, _, err := g.Fetch(context.Background(), "a/b", 1); err == nil {
			t.Fatalf("accepted malformed checks %s", payload)
		}
	}
	g := githubStub(t, map[string]string{"repos/a/b/pulls/1": prJSON("head", ""), "repos/a/b/actions/runs?head_sha=head&per_page=100": noWorkflowRuns, "repos/a/b/commits/head/check-runs?filter=all&per_page=100": `[{"total_count":0,"check_runs":[]}]`})
	if pr, _, err := g.Fetch(context.Background(), "a/b", 1); err == nil || pr.HeadSHA != "head" {
		t.Fatalf("missing statuses should retain metadata and fail: %+v %v", pr, err)
	}
}

func TestGitHubRejectsHeadChangedDuringCheckCollection(t *testing.T) {
	base := githubStub(t, map[string]string{"repos/a/b/pulls/1": prJSON("old", ""), "repos/a/b/actions/runs?head_sha=old&per_page=100": noWorkflowRuns, "repos/a/b/commits/old/check-runs?filter=all&per_page=100": `[{"total_count":0,"check_runs":[]}]`, "repos/a/b/commits/old/statuses?per_page=100": `[[]]`})
	state := filepath.Join(t.TempDir(), "seen")
	cli := filepath.Join(t.TempDir(), "gh")
	script := "#!/bin/sh\nif [ \"$4\" = 'repos/a/b/pulls/1' ]; then\nif [ -f " + shellLiteral(state) + " ]; then printf '%s\\n' " + shellLiteral(prJSON("new", "")) + "; exit 0; fi\ntouch " + shellLiteral(state) + "\nfi\nexec " + shellLiteral(base.CLI) + " \"$@\"\n"
	if err := os.WriteFile(cli, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	pr, _, err := (GitHub{CLI: cli}).Fetch(context.Background(), "a/b", 1)
	if err == nil || !strings.Contains(err.Error(), "changed") || pr.HeadSHA != "new" {
		t.Fatalf("stale snapshot accepted: %+v %v", pr, err)
	}
}
