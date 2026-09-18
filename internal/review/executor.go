package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"text/template"
)

type ToolArgs struct {
	Repo       string
	PRNumber   int
	PRURL      string
	Title      string
	BaseBranch string
	Worktree   string
	Guide      string
	ReviewID   string
	HeadCommit string
	// SessionID, when set, makes the rendered command resume that tool session
	// (opencode run --session <id>) so a retry/continue appends to the previous
	// review conversation instead of starting fresh.
	SessionID string
	// RetryNote, when set, replaces the initial review prompt with a
	// continuation instruction; it is shellquoted into the outer prompt.
	RetryNote string
}

const maxResultBytes = 512 * 1024

var DefaultCommandTemplate = `run {{if .SessionID}}--session {{.SessionID}} {{end}}--title "saturnus-review-{{.ReviewID}}" "{{if .RetryNote}}{{.RetryNote | shellquote}}{{else}}Review GitHub PR {{.Repo}}#{{.PRNumber}} ({{.PRURL}}). The repository is already checked out at {{.Worktree}}. Fetch and check out the PR head yourself, then review the changes. Focus on correctness, security and style; be concise with file:line references.{{if .Guide}} Review guidelines: {{.Guide | shellquote}}{{end}}{{end}} Finish with a structured review. The very last block must be exactly:

REVIEW SUMMARY:
PR summary: <what the PR does in 2-3 sentences>
Issues: <each concrete problem with file:line references, one per line, or None>
Review suggestions: <verdict and next steps: LGTM, or the specific changes required, or a design/refactor suggestion>"`

func RenderCommand(templateText string, args ToolArgs) ([]string, error) {
	tmpl, err := template.New("review").Funcs(template.FuncMap{"shellquote": shellquote}).Parse(templateText)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, args); err != nil {
		return nil, err
	}
	return splitCommand(buf.String())
}

func shellquote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func splitCommand(line string) ([]string, error) {
	var out []string
	var cur strings.Builder
	quote := rune(0)
	escaped := false
	for _, r := range line {
		if escaped {
			cur.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case ' ', '\t', '\n':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if escaped {
		return nil, errors.New("trailing escape in command template")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unclosed quote in command template")
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out, nil
}

// Run executes the review tool in dir, streaming stdout/stderr to sink and
// returning the combined output. The tool's stdout and stderr are wired to
// writers, so os/exec drains them before Wait returns; using StdoutPipe here
// instead would race with Wait and occasionally lose buffered output.
func Run(ctx context.Context, tool string, args []string, dir string, sink func(string)) (string, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Dir = dir
	setupProcessGroup(cmd)

	var mu sync.Mutex
	var buf strings.Builder
	w := &sinkWriter{buf: &buf, mu: &mu, sink: sink}
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		return "", err
	}

	runErr := cmd.Wait()

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if len(out) > maxResultBytes {
		out = out[:maxResultBytes] + "\n...[truncated]"
	}
	return out, runErr
}

// sinkWriter accumulates output (bounded to maxResultBytes) and forwards every
// chunk to the streaming sink.
type sinkWriter struct {
	mu   *sync.Mutex
	buf  *strings.Builder
	sink func(string)
}

func (w *sinkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.buf.Len() < maxResultBytes {
		_, _ = w.buf.Write(p)
	}
	w.mu.Unlock()
	if w.sink != nil {
		w.sink(string(p))
	}
	return len(p), nil
}
