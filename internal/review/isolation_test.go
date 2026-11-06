package review

import (
	"path/filepath"
	"strings"
	"testing"

	"saturnus/internal/repopolicy"
)

func TestWorktreePromptForFreshRetryAndCustomCommands(t *testing.T) {
	root := filepath.Join(t.TempDir(), `checkout with "quotes"`)
	for _, tc := range []struct{ name, tmpl, retry string }{
		{"fresh", DefaultCommandTemplate, ""},
		{"retry", DefaultCommandTemplate, "Continue the review"},
		{"custom", `run --title custom "Old template: fetch and check out the PR head here"`, ""},
		{"custom explicit", `run "{{.IsolationPrompt | shellquote}}Custom instructions"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := RenderCommand(tc.tmpl, ToolArgs{Repo: "a/b", ReviewID: "rvw_one", Worktree: root, RepoCollisionPolicy: repopolicy.Worktree, RetryNote: tc.retry})
			if err != nil {
				t.Fatal(err)
			}
			prompt := args[len(args)-1]
			for _, want := range []string{"git worktree add --detach", ".saturnus-worktrees", "rvw_one", "Never switch branches", "stop and report the failure", "Keep this review's worktree"} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("missing %q in %s", want, prompt)
				}
			}
			if strings.Count(prompt, "Workspace isolation rules") != 1 {
				t.Fatalf("duplicated isolation prompt: %s", prompt)
			}
			if !strings.Contains(prompt, IsolationPrompt(repopolicy.Worktree, root, "rvw_one")) {
				t.Fatal("path escaping corrupted isolation prompt")
			}
		})
	}
	if IsolationPrompt(repopolicy.Worktree, root, "rvw_one") == IsolationPrompt(repopolicy.Worktree, root, "rvw_two") {
		t.Fatal("different reviews must use different worktrees")
	}
	args, err := RenderCommand(DefaultCommandTemplate, ToolArgs{RepoCollisionPolicy: repopolicy.Mutex})
	if err != nil || strings.Contains(args[len(args)-1], "Workspace isolation rules") {
		t.Fatalf("mutex prompt: %v %v", args, err)
	}
}
