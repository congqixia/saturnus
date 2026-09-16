package review

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
}

const maxResultBytes = 512 * 1024

var DefaultCommandTemplate = `run "Review GitHub PR {{.Repo}}#{{.PRNumber}} ({{.PRURL}}). The repository is already checked out at {{.Worktree}}. Fetch and check out the PR head yourself, then review the changes. Focus on correctness, security and style; be concise with file:line references.{{if .Guide}} Review guidelines: {{.Guide | shellquote}}{{end}} End your review with a line starting with REVIEW SUMMARY: followed by your conclusion."`

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

func Run(ctx context.Context, tool string, args []string, dir string, sink func(string)) (string, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Dir = dir
	setupProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	var mu sync.Mutex
	var buf strings.Builder
	emit := func(line string) {
		mu.Lock()
		if buf.Len() < maxResultBytes {
			buf.WriteString(line)
		}
		mu.Unlock()
		if sink != nil {
			sink(line)
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanLines(stdout, emit)
	}()
	go func() {
		defer wg.Done()
		scanLines(stderr, emit)
	}()
	runErr := cmd.Wait()
	wg.Wait()

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if len(out) > maxResultBytes {
		out = out[:maxResultBytes] + "\n...[truncated]"
	}
	return out, runErr
}

func scanLines(r io.Reader, emit func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		emit(sc.Text() + "\n")
	}
}
