package tasks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeGHTimeline writes an executable "gh" stub answering the
// `api repos/{repo}/issues/{num}/timeline --paginate` call with the given
// timeline JSON, and failing (exit 1) for every other invocation.
func fakeGHTimeline(t *testing.T, endpoint, timelineJSON string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "gh")
	content := `#!/bin/sh
if [ "$1" = "api" ] && [ "$2" = "` + endpoint + `" ]; then
  printf '%s\n' '` + timelineJSON + `'
  exit 0
fi
echo "unexpected gh call: $*" >&2
exit 1
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestResolveLinkedPRs(t *testing.T) {
	gh := fakeGHTimeline(t, "repos/a/b/issues/10/timeline?per_page=100", `[
		{"event":"labeled","actor":{"login":"x"}},
		{"event":"cross-referenced","source":{"type":"issue","issue":{
			"number":7,"repository_url":"https://api.github.com/repos/a/b",
			"pull_request":{"url":"https://api.github.com/repos/a/b/pulls/7"}}}},
		{"event":"cross-referenced","source":{"type":"issue","issue":{
			"number":8,"repository_url":"https://api.github.com/repos/a/b",
			"pull_request":{"url":"https://api.github.com/repos/a/b/pulls/8"}}}},
		{"event":"cross-referenced","source":{"type":"issue","issue":{
			"number":7,"repository_url":"https://api.github.com/repos/a/b",
			"pull_request":{"url":"https://api.github.com/repos/a/b/pulls/7"}}}},
		{"event":"cross-referenced","source":{"type":"issue","issue":{
			"number":99,"repository_url":"https://api.github.com/repos/other/repo",
			"pull_request":{"url":"https://api.github.com/repos/other/repo/pulls/99"}}}},
		{"event":"cross-referenced","source":{"type":"issue","issue":{
			"number":100,"repository_url":"https://api.github.com/repos/a/b"}}}
	]`)

	prs := ResolveLinkedPRs(context.Background(), gh, "a/b", 10)
	if len(prs) != 2 {
		t.Fatalf("expected 2 linked PRs, got %#v", prs)
	}
	if prs[0].Number != 7 || prs[0].URL != "https://github.com/a/b/pull/7" {
		t.Fatalf("unexpected first PR: %#v", prs[0])
	}
	if prs[1].Number != 8 || prs[1].URL != "https://github.com/a/b/pull/8" {
		t.Fatalf("unexpected second PR: %#v", prs[1])
	}
}

func TestResolveLinkedPRsFiltersNonCrossReferencedAndForeignRepos(t *testing.T) {
	gh := fakeGHTimeline(t, "repos/a/b/issues/10/timeline?per_page=100", `[
		{"event":"cross-referenced","source":{"type":"issue","issue":{
			"number":7,"repository_url":"https://api.github.com/repos/a/b",
			"pull_request":{"url":"https://api.github.com/repos/a/b/pulls/7"}}}},
		{"event":"cross-referenced","source":{"type":"issue","issue":{
			"number":5,"repository_url":"https://api.github.com/repos/a/b"}}},
		{"event":"connected","actor":{"login":"x"}}
	]`)

	prs := ResolveLinkedPRs(context.Background(), gh, "a/b", 10)
	if len(prs) != 1 || prs[0].Number != 7 {
		t.Fatalf("expected only PR 7, got %#v", prs)
	}
}

func TestResolveLinkedPRsNoGHCliOrFailure(t *testing.T) {
	if prs := ResolveLinkedPRs(context.Background(), "", "a/b", 10); len(prs) != 0 {
		t.Fatalf("empty gh cli should return nothing, got %#v", prs)
	}
	gh := fakeGHTimeline(t, "repos/a/b/issues/10/timeline?per_page=100", `{"unexpected":"shape"}`)
	if prs := ResolveLinkedPRs(context.Background(), gh, "a/b", 10); len(prs) != 0 {
		t.Fatalf("non-array response should return nothing, got %#v", prs)
	}
}
