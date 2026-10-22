package review

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fakeGHListOpen writes an executable "gh" stub answering `pr list` with the
// given JSON per repo, and failing (exit 1) for repos not covered.
func fakeGHListOpen(t *testing.T, responses map[string]string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "gh")
	repos := make([]string, 0, len(responses))
	for repo := range responses {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	for _, repo := range repos {
		b.WriteString("if [ \"$4\" = \"" + repo + "\" ]; then printf '%s\\n' '" + responses[repo] + "'; exit 0; fi\n")
	}
	b.WriteString("exit 1\n")
	if err := os.WriteFile(script, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestListOpenPRsScopesToWhitelistedRepos(t *testing.T) {
	gh := fakeGHListOpen(t, map[string]string{
		"a/b": `[{"number":7,"title":"A","headRefName":"feat/a","baseRefName":"main","isDraft":true,"url":"https://github.com/a/b/pull/7","updatedAt":"2026-09-24T10:00:00Z"},{"number":8,"title":"B","headRefName":"feat/b","baseRefName":"release","isDraft":false,"url":"https://github.com/a/b/pull/8","updatedAt":"2026-09-24T11:00:00Z"}]`,
		// c/d is not covered by the stub: its gh lookup must be reported as an
		// error instead of hiding a/b's results.
	})

	prs, errs := ListOpenPRs(context.Background(), gh, []string{"a/b", "c/d"})
	if len(prs) != 2 {
		t.Fatalf("expected 2 PRs, got %#v", prs)
	}
	byNumber := map[int]OpenPR{}
	for _, pr := range prs {
		byNumber[pr.Number] = pr
	}
	pr := byNumber[7]
	if pr.Repo != "a/b" || pr.Title != "A" || pr.BaseBranch != "main" || pr.HeadBranch != "feat/a" || !pr.IsDraft {
		t.Fatalf("unexpected PR: %#v", pr)
	}
	pr = byNumber[8]
	if pr.BaseBranch != "release" || pr.IsDraft {
		t.Fatalf("unexpected PR: %#v", pr)
	}
	if _, ok := errs["c/d"]; !ok {
		t.Fatalf("expected error for c/d, got %v", errs)
	}
}

func TestListOpenPRsNoReposOrNoGHCli(t *testing.T) {
	prs, errs := ListOpenPRs(context.Background(), "", []string{"a/b"})
	if len(prs) != 0 || len(errs) != 0 {
		t.Fatalf("empty gh cli should return nothing, got prs=%#v errs=%#v", prs, errs)
	}
}
