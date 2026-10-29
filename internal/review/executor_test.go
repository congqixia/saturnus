package review

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		want    []string
		wantErr bool
	}{
		{name: "simple", line: "run hello", want: []string{"run", "hello"}},
		{name: "quoted", line: `run "hello world"`, want: []string{"run", "hello world"}},
		{name: "single quote", line: `run 'a b' c`, want: []string{"run", "a b", "c"}},
		{name: "escaped space", line: `run a\ b`, want: []string{"run", "a b"}},
		{name: "unclosed quote", line: `run "oops`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitCommand(tc.line)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %#v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestRenderCommandContextTitlePreservesLegacyCustomization(t *testing.T) {
	for _, contextID := range []string{"", "ctx_new"} {
		title := SessionTitle("rvw", contextID)
		args, err := RenderCommand(`run --title custom-review prompt`, ToolArgs{ReviewID: "rvw", SessionTitle: title})
		if err != nil {
			t.Fatal(err)
		}
		want := "custom-review"
		if contextID != "" {
			want = title
		}
		if len(args) != 4 || args[2] != want {
			t.Fatalf("context=%s args=%v want title=%s", contextID, args, want)
		}
	}
}

func TestRenderCommand(t *testing.T) {
	args := ToolArgs{Repo: "congqixia/saturnus", PRNumber: 42, PRURL: "https://github.com/congqixia/saturnus/pull/42", ReviewID: "rvw_x"}
	got, err := RenderCommand(DefaultCommandTemplate, args)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[0] != "run" || got[1] != "--auto" || got[2] != "--title" {
		t.Fatalf("unexpected args: %#v", got)
	}
	if got[3] != "saturnus-review-rvw_x" {
		t.Fatalf("expected title arg, got %q", got[3])
	}
	if got[4] == "" {
		t.Fatal("expected non-empty prompt")
	}
	for _, want := range []string{"REVIEW SUMMARY:", "PR summary:", "Issues:", "Review suggestions:"} {
		if !strings.Contains(got[4], want) {
			t.Fatalf("prompt missing %q: %q", want, got[4])
		}
	}
}

func TestRenderCommandWithGuide(t *testing.T) {
	args := ToolArgs{
		Repo:     "milvus-io/milvus",
		PRNumber: 1,
		PRURL:    "https://github.com/milvus-io/milvus/pull/1",
		Worktree: "/tmp/milvus",
		ReviewID: "rvw_y",
		Guide:    "Do NOT run compilation.\nKeep comments in \"English\".",
	}
	got, err := RenderCommand(DefaultCommandTemplate, args)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 args, got %#v", got)
	}
	prompt := got[4]
	if !strings.Contains(prompt, "Do NOT run compilation.\nKeep comments in \"English\".") {
		t.Fatalf("guide not embedded correctly: %q", prompt)
	}
	if strings.Contains(prompt, `\"`) || strings.Contains(prompt, `\\`) {
		t.Fatalf("guide should be unescaped after splitCommand: %q", prompt)
	}
}

func TestRenderCommandContinueSession(t *testing.T) {
	args := ToolArgs{
		Repo:      "a/b",
		PRNumber:  1,
		PRURL:     "https://github.com/a/b/pull/1",
		ReviewID:  "rvw_c",
		SessionID: "ses_9",
		RetryNote: "Continue reviewing. Reason: retry after failure.",
	}
	got, err := RenderCommand(DefaultCommandTemplate, args)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 || got[0] != "run" || got[1] != "--auto" || got[2] != "--session" || got[3] != "ses_9" || got[4] != "--title" {
		t.Fatalf("expected session continuation args, got %#v", got)
	}
	if got[5] != "saturnus-review-rvw_c" {
		t.Fatalf("expected title arg, got %q", got[5])
	}
	prompt := got[6]
	if !strings.Contains(prompt, "Continue reviewing. Reason: retry after failure.") {
		t.Fatalf("retry note missing from prompt: %q", prompt)
	}
	if strings.Contains(prompt, "Review GitHub PR a/b#1") {
		t.Fatalf("retry prompt must not contain the fresh-review prompt: %q", prompt)
	}
	if !strings.Contains(prompt, "REVIEW SUMMARY:") {
		t.Fatalf("retry prompt must still require the structured summary: %q", prompt)
	}
}

func TestShellquote(t *testing.T) {
	if got := shellquote(`a"b\c`); got != `a\"b\\c` {
		t.Fatalf("shellquote = %q", got)
	}
}

func TestRenderCommandWithResultAPI(t *testing.T) {
	args := ToolArgs{
		Repo:         "a/b",
		PRNumber:     7,
		PRURL:        "https://github.com/a/b/pull/7",
		ReviewID:     "rvw_api",
		ResultAPIURL: "http://127.0.0.1:8787/api/reviews/rvw_api/result",
		ResultToken:  "s3cret",
	}
	got, err := RenderCommand(DefaultCommandTemplate, args)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("expected 5 args, got %#v", got)
	}
	prompt := got[4]
	for _, want := range []string{
		"http://127.0.0.1:8787/api/reviews/rvw_api/result",
		`Authorization: Bearer s3cret`,
		"\"verdict\": \"lgtm\"",
		"\"severity\": \"P0\"",
		"request_change",
		"nit_comments",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	// The prompt must not leak unescaped quotes or backslashes after splitting.
	if strings.Contains(prompt, `\"`) || strings.Contains(prompt, `\\`) {
		t.Fatalf("prompt still contains escape sequences: %q", prompt)
	}
}

func TestRenderCommandWithoutResultAPIOmitsPostBlock(t *testing.T) {
	args := ToolArgs{Repo: "a/b", PRNumber: 1, PRURL: "https://github.com/a/b/pull/1", ReviewID: "rvw_x"}
	got, err := RenderCommand(DefaultCommandTemplate, args)
	if err != nil {
		t.Fatal(err)
	}
	prompt := got[4]
	for _, absent := range []string{"machine-readable", "Authorization: Bearer", `\"verdict\"`, "Previous findings"} {
		if strings.Contains(prompt, absent) {
			t.Fatalf("prompt should not contain %q:\n%s", absent, prompt)
		}
	}
}

func TestRenderCommandCarriesPriorFindings(t *testing.T) {
	args := ToolArgs{
		Repo:          "a/b",
		PRNumber:      3,
		PRURL:         "https://github.com/a/b/pull/3",
		ReviewID:      "rvw_p",
		ResultAPIURL:  "http://127.0.0.1:8787/api/reviews/rvw_p/result",
		PriorFindings: "- fnd_x [P0] blocking bug (a.go:1)\n  status: open\n",
	}
	got, err := RenderCommand(DefaultCommandTemplate, args)
	if err != nil {
		t.Fatal(err)
	}
	prompt := got[4]
	for _, want := range []string{
		"Previous findings from the earlier round",
		"- fnd_x [P0] blocking bug (a.go:1)",
		"status: open",
		"\"id\": \"<prior finding id, only for status updates>\"",
		"\"status\": \"open\" | \"addressed\" | \"commented\"",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}
