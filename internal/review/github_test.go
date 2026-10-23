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

// fakeGHPost writes an executable "gh" stub for the `api ... --method POST
// --input -` calls. It appends "method endpoint payload" to $FGH_LOG per call
// and can be told to fail (exit 1) for requests whose body contains $FGH_FAIL.
type fakeGHPost struct {
	failBody string
}

func (f *fakeGHPost) ghScript(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "gh")
	content := `#!/bin/sh
endpoint=""
for arg in "$@"; do
  case "$arg" in
    repos/*) endpoint="$arg" ;;
  esac
done
input="$(cat)"
printf 'POST %s %s\n' "$endpoint" "$input" >>"$FGH_LOG"
if [ -n "$FGH_FAIL" ] && printf '%s' "$input" | grep -q "$FGH_FAIL"; then
  echo "mock failure" >&2
  exit 1
fi
printf '%s\n' '{"id": 1}'
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestPostInlineCommentPostsJSON(t *testing.T) {
	f := &fakeGHPost{}
	gh := f.ghScript(t)
	log := filepath.Join(t.TempDir(), "gh.log")
	t.Setenv("FGH_LOG", log)

	commentID, err := PostInlineComment(context.Background(), gh, "a/b", 7, "abc1234", InlineComment{
		Path: "src/foo.go", Line: 42, Body: "see issue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if commentID == "" {
		t.Fatal("expected the gh response comment id to be returned")
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(data)), " ", 3)
	if parts[0] != "POST" || parts[1] != "repos/a/b/pulls/7/comments" {
		t.Fatalf("unexpected gh call: %q", parts)
	}
	for _, want := range []string{`"path":"src/foo.go"`, `"line":42`, `"side":"RIGHT"`, `"commit_id":"abc1234"`, `"body":"see issue"`} {
		if !strings.Contains(parts[2], want) {
			t.Fatalf("payload missing %q: %q", want, parts[2])
		}
	}
}

func TestPostInlineCommentSkipsInvalidAndSurfacesGHFailure(t *testing.T) {
	if _, err := PostInlineComment(context.Background(), "", "a/b", 7, "", InlineComment{Path: "a.go", Line: 1, Body: "b"}); err == nil {
		t.Fatal("expected error for empty gh cli")
	}
	if _, err := PostInlineComment(context.Background(), "gh", "a/b", 7, "", InlineComment{Path: "", Line: 1, Body: "b"}); err == nil {
		t.Fatal("expected error for empty path")
	}
	if _, err := PostInlineComment(context.Background(), "gh", "a/b", 7, "", InlineComment{Path: "a.go", Line: 0, Body: "b"}); err == nil {
		t.Fatal("expected error for zero line")
	}

	f := &fakeGHPost{failBody: "a.go"}
	gh := f.ghScript(t)
	t.Setenv("FGH_LOG", filepath.Join(t.TempDir(), "gh.log"))
	t.Setenv("FGH_FAIL", f.failBody)
	_, err := PostInlineComment(context.Background(), gh, "a/b", 7, "", InlineComment{Path: "a.go", Line: 9, Body: "boom"})
	if err == nil || !strings.Contains(err.Error(), "a.go:9") || !strings.Contains(err.Error(), "mock failure") {
		t.Fatalf("expected gh failure surfaced, got %v", err)
	}
}

func TestPostPRReviewPostsEvent(t *testing.T) {
	f := &fakeGHPost{}
	gh := f.ghScript(t)
	log := filepath.Join(t.TempDir(), "gh.log")
	t.Setenv("FGH_LOG", log)

	if err := PostPRReview(context.Background(), gh, "a/b", 7, "REQUEST_CHANGES", "please fix"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(data)), " ", 3)
	if parts[0] != "POST" || parts[1] != "repos/a/b/pulls/7/reviews" {
		t.Fatalf("unexpected gh call: %q", parts)
	}
	if !strings.Contains(parts[2], `"event":"REQUEST_CHANGES"`) || !strings.Contains(parts[2], `"body":"please fix"`) {
		t.Fatalf("unexpected payload: %q", parts[2])
	}
	if err := PostPRReview(context.Background(), "", "a/b", 7, "COMMENT", "x"); err == nil {
		t.Fatal("expected error for empty gh cli")
	}
}

// fakeGHResolve writes a "gh" stub answering the two GraphQL calls that
// ResolveInlineComment makes: a thread lookup and the resolveReviewThread
// mutation. The lookup returns a thread whose comments include the given
// databaseId; the mutation returns the resolved thread.
func fakeGHResolve(t *testing.T, commentID, threadID string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "gh")
	lookup := `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"T_1","isResolved":false,"comments":{"nodes":[{"databaseId":` + commentID + `}]}},{"id":"T_2","isResolved":true,"comments":{"nodes":[{"databaseId":999}]}}]}}}}}`
	resolved := `{"data":{"resolveReviewThread":{"thread":{"id":"` + threadID + `","isResolved":true}}}`
	content := `#!/bin/sh
case "$*" in
  *reviewThreads*) printf '%s\n' '` + lookup + `' ;;
  *resolveReviewThread*) printf '%s\n' '` + resolved + `' ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestResolveInlineComment(t *testing.T) {
	gh := fakeGHResolve(t, "123456", "T_1")
	if err := ResolveInlineComment(context.Background(), gh, "a/b", 7, "123456"); err != nil {
		t.Fatal(err)
	}
	// The thread is already resolved or missing -> no-op, no error.
	if err := ResolveInlineComment(context.Background(), gh, "a/b", 7, "999999"); err != nil {
		t.Fatalf("expected no-op for missing thread, got %v", err)
	}
	// Validation.
	if err := ResolveInlineComment(context.Background(), "", "a/b", 7, "1"); err == nil {
		t.Fatal("expected error for empty gh cli")
	}
	if err := ResolveInlineComment(context.Background(), "gh", "a/b", 7, ""); err == nil {
		t.Fatal("expected error for empty comment id")
	}
}
