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

func TestRenderCommand(t *testing.T) {
	args := ToolArgs{Repo: "congqixia/saturnus", PRNumber: 42, PRURL: "https://github.com/congqixia/saturnus/pull/42", ReviewID: "rvw_x"}
	got, err := RenderCommand(DefaultCommandTemplate, args)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0] != "run" || got[1] != "--title" {
		t.Fatalf("unexpected args: %#v", got)
	}
	if got[2] != "saturnus-review-rvw_x" {
		t.Fatalf("expected title arg, got %q", got[2])
	}
	if got[3] == "" {
		t.Fatal("expected non-empty prompt")
	}
	for _, want := range []string{"REVIEW SUMMARY:", "PR summary:", "Issues:", "Review suggestions:"} {
		if !strings.Contains(got[3], want) {
			t.Fatalf("prompt missing %q: %q", want, got[3])
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
	if len(got) != 4 {
		t.Fatalf("expected 4 args, got %#v", got)
	}
	prompt := got[3]
	if !strings.Contains(prompt, "Do NOT run compilation.\nKeep comments in \"English\".") {
		t.Fatalf("guide not embedded correctly: %q", prompt)
	}
	if strings.Contains(prompt, `\"`) || strings.Contains(prompt, `\\`) {
		t.Fatalf("guide should be unescaped after splitCommand: %q", prompt)
	}
}

func TestShellquote(t *testing.T) {
	if got := shellquote(`a"b\c`); got != `a\"b\\c` {
		t.Fatalf("shellquote = %q", got)
	}
}
