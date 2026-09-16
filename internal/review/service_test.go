package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"saturnus/internal/lark"
	"saturnus/internal/store"
)

func TestServiceSubmitFlow(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"congqixia/saturnus": t.TempDir()},
	})
	if !svc.Enabled() {
		t.Fatal("expected review service enabled")
	}

	req, err := svc.Submit(MessageContext{
		Text:         "please review PR https://github.com/congqixia/saturnus/pull/42",
		ChatID:       "oc_chat",
		SenderOpenID: "ou_me",
		MessageID:    "om_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != "pending" || req.Repo != "congqixia/saturnus" || req.PRNumber != 42 {
		t.Fatalf("unexpected review: %#v", req)
	}

	got, err := svc.Get(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ThreadID == "" || got.ChatID != "oc_chat" {
		t.Fatalf("thread context missing: %#v", got)
	}

	list := svc.List("pending")
	if len(list) != 1 {
		t.Fatalf("expected 1 pending review, got %d", len(list))
	}
	if svc.FormatList(list) == "" || svc.FormatOne(got) == "" {
		t.Fatal("expected non-empty formatted output")
	}
}

func TestServiceSubmitRejectsUnauthorizedAndDisabled(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	disabled := New(st, lark.New(lark.Config{}), Config{Enabled: false})
	if _, err := disabled.Submit(MessageContext{Text: "review https://github.com/a/b/pull/1", SenderOpenID: "ou_me"}); err == nil {
		t.Fatal("expected error for disabled service")
	}

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_allowed"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	if _, err := svc.Submit(MessageContext{Text: "review https://github.com/a/b/pull/1", SenderOpenID: "ou_other"}); err == nil {
		t.Fatal("expected error for unauthorized sender")
	}
	if _, err := svc.Submit(MessageContext{Text: "no pr url here", SenderOpenID: "ou_allowed"}); err == nil {
		t.Fatal("expected error for missing PR URL")
	}
}

func TestServiceDeduplicatesPending(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	ctx := MessageContext{
		Text:         "review https://github.com/a/b/pull/5",
		ChatID:       "oc_chat",
		SenderOpenID: "ou_me",
		MessageID:    "om_1",
	}
	if _, err := svc.Submit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(ctx); err == nil {
		t.Fatal("expected dedupe error for same pending PR")
	}
}

func TestServiceRejectsNonWhitelistedRepo(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"allowed/repo": t.TempDir()},
	})
	if _, err := svc.Submit(MessageContext{
		Text:         "review https://github.com/unknown/repo/pull/9",
		ChatID:       "oc_chat",
		SenderOpenID: "ou_me",
	}); err == nil {
		t.Fatal("expected error for repo outside the whitelist")
	}
	if _, err := svc.Submit(MessageContext{
		Text:         "review https://github.com/allowed/repo/pull/9",
		ChatID:       "oc_chat",
		SenderOpenID: "ou_me",
	}); err != nil {
		t.Fatalf("expected whitelisted repo to be accepted, got %v", err)
	}
}

func TestServiceAllowAllUsers(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"*"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	if _, err := svc.Submit(MessageContext{
		Text:         "review https://github.com/a/b/pull/3",
		ChatID:       "oc_chat",
		SenderOpenID: "ou_whoever",
	}); err != nil {
		t.Fatalf("expected wildcard allow-all to accept any sender, got %v", err)
	}
}

func TestServiceProcessRunsToolInRepoDir(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "tool.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npwd\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"a/b": repoDir},
		Tool:         script,
	})
	req, err := svc.Submit(MessageContext{
		Text:         "review https://github.com/a/b/pull/5",
		ChatID:       "oc_chat",
		SenderOpenID: "ou_me",
		MessageID:    "om_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.process(req.ID)

	got, err := svc.Get(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status = %q, want succeeded; error=%q result=%q", got.Status, got.Error, got.ResultText)
	}
	if strings.TrimSpace(got.ResultText) != repoDir {
		t.Fatalf("tool ran in %q, want repo dir %q", strings.TrimSpace(got.ResultText), repoDir)
	}
}

func TestLoadGuide(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".saturnus-review.md"), []byte("in-repo guide: no compile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(t.TempDir(), "guide.txt")
	if err := os.WriteFile(explicit, []byte("explicit guide: read only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	otherRepo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(otherRepo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	svc := New(nil, lark.New(lark.Config{}), Config{})
	got, err := svc.loadGuide("a/b", repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "in-repo guide: no compile" {
		t.Fatalf("in-repo discovery got %q", got)
	}

	svc.cfg.Guides = map[string]string{"a/b": explicit}
	got, err = svc.loadGuide("a/b", repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "explicit guide: read only" {
		t.Fatalf("explicit guide got %q", got)
	}

	got, err = svc.loadGuide("other/repo", otherRepo)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("expected empty guide for checkout without guide file, got %q", got)
	}
}

func TestServiceSubmitRerunsTerminatedReview(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	ctx := MessageContext{
		Text:         "review https://github.com/a/b/pull/9",
		ChatID:       "oc_c",
		SenderOpenID: "ou_me",
		MessageID:    "om_1",
	}
	req, err := svc.Submit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req.Status = "succeeded"
	req.ResultText = "done"
	req.CompletedAt = time.Now()
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}

	req2, err := svc.Submit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if req2.ID != req.ID {
		t.Fatalf("expected same review id for re-run, got %q want %q", req2.ID, req.ID)
	}
	if req2.Status != "pending" {
		t.Fatalf("expected reset to pending, got %q", req2.Status)
	}
	if req2.ResultText != "" {
		t.Fatalf("expected cleared result, got %q", req2.ResultText)
	}
}

func TestServiceSubmitRejectsActiveDuplicate(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	ctx := MessageContext{
		Text:         "review https://github.com/a/b/pull/5",
		ChatID:       "oc_c",
		SenderOpenID: "ou_me",
		MessageID:    "om_1",
	}
	if _, err := svc.Submit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(ctx); err != ErrAlreadyPending {
		t.Fatalf("expected ErrAlreadyPending for active duplicate, got %v", err)
	}
}

func TestIsReviewer(t *testing.T) {
	svc := New(nil, lark.New(lark.Config{}), Config{
		AllowedUsers:   []string{"ou_a"},
		ReviewerOpenID: "ou_r",
	})
	if !svc.isReviewer("ou_r") {
		t.Fatal("expected configured reviewer")
	}
	if !svc.isReviewer("ou_a") {
		t.Fatal("expected allowed user to be treated as reviewer")
	}
	if svc.isReviewer("ou_x") {
		t.Fatal("expected unrelated user rejected")
	}
	if svc.isReviewer("") {
		t.Fatal("expected empty sender rejected")
	}
}

func TestCaptureSessionID(t *testing.T) {
	repoDir := t.TempDir()
	script := filepath.Join(t.TempDir(), "tool.sh")
	content := `#!/bin/sh
cat <<'EOF'
[{"id":"ses_new","title":"saturnus-review-rvw_x","directory":"` + repoDir + `","updated":2000},{"id":"ses_old","title":"other","directory":"` + repoDir + `","updated":1000},{"id":"ses_otherdir","title":"saturnus-review-rvw_x","directory":"/elsewhere","updated":3000}]
EOF
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := New(nil, lark.New(lark.Config{}), Config{Tool: script})

	if got := svc.captureSessionID(repoDir, "rvw_x"); got != "ses_new" {
		t.Fatalf("title match = %q, want ses_new", got)
	}
	if got := svc.captureSessionID(repoDir, "rvw_z"); got != "ses_new" {
		t.Fatalf("fallback by dir recency = %q, want ses_new", got)
	}
}

func TestTaskMembers(t *testing.T) {
	got := taskMembers("ou_r", "ou_q")
	if len(got) != 2 || got[0] != "ou_r" || got[1] != "ou_q" {
		t.Fatalf("taskMembers = %#v", got)
	}
	got = taskMembers("ou_same", "ou_same")
	if len(got) != 1 || got[0] != "ou_same" {
		t.Fatalf("expected dedupe, got %#v", got)
	}
	got = taskMembers("", "")
	if len(got) != 0 {
		t.Fatalf("expected empty members, got %#v", got)
	}
}

func TestParseRepoMap(t *testing.T) {
	m := ParseRepoMap(" a/b=/srv/a , c/d = /srv/c ,malformed,e/f=/srv/e")
	want := map[string]string{"a/b": "/srv/a", "c/d": "/srv/c", "e/f": "/srv/e"}
	if len(m) != len(want) {
		t.Fatalf("got %d entries, want %d: %#v", len(m), len(want), m)
	}
	for repo, dir := range want {
		if m[repo] != dir {
			t.Fatalf("repo %s -> %q, want %q", repo, m[repo], dir)
		}
	}
}
