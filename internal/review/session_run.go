package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// SessionIdentity names the conversation this process is allowed to use.
// SessionID is empty only when a new root session is requested.
type SessionIdentity struct {
	ReviewID, ContextID, SessionID string
}

type RunResult struct {
	Output    string
	SessionID string
}

// RunWithSession binds only the root ID emitted on this process's stdout.
// Stderr, nested tool output and the global recent-session list cannot provide
// an ID. Existing sessions are checked before the process is allowed to resume.
func RunWithSession(ctx context.Context, tool string, args []string, dir string, identity SessionIdentity, sink func(string)) (RunResult, error) {
	var result RunResult
	jsonMode, err := checkSessionArgs(args, dir, identity.SessionID)
	if err != nil {
		return result, err
	}
	if identity.SessionID != "" {
		if err := ValidateSession(ctx, tool, dir, identity.ReviewID, identity.ContextID, identity.SessionID); err != nil {
			return result, err
		}
		result.SessionID = identity.SessionID
	}
	if !jsonMode {
		// Custom text commands remain supported, but their output is never used
		// to guess a new session. To capture new IDs they must use JSON events.
		result.Output, err = Run(ctx, tool, args, dir, sink)
		return result, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var buf strings.Builder
	w := &sinkWriter{mu: &mu, buf: &buf, sink: sink}
	stream := &sessionStream{write: func(s string) { _, _ = w.Write([]byte(s)) }, expected: identity.SessionID, cancel: cancel}
	runErr := runProcess(runCtx, tool, args, dir, stream, w)
	stream.finish()
	result.Output = buf.String()
	if len(result.Output) > maxResultBytes {
		result.Output = result.Output[:maxResultBytes] + "\n...[truncated]"
	}
	if stream.err != nil {
		return result, errors.Join(stream.err, runErr)
	}
	runErr = errors.Join(runErr, stream.toolErr)
	if stream.sessionID != "" && identity.SessionID == "" {
		// Read-only cleanup is separately bounded, so a timeout can still keep
		// this process's verified session for a later retry.
		if err := ValidateSession(context.WithoutCancel(ctx), tool, dir, identity.ReviewID, identity.ContextID, stream.sessionID); err != nil {
			return result, errors.Join(err, runErr)
		}
		result.SessionID = stream.sessionID
	}
	return result, runErr
}

// Reject implicit or template-hardcoded session selection. Otherwise an old
// custom --continue/--session could resume the wrong conversation before the
// first JSON event gives us any chance to detect it.
func checkSessionArgs(args []string, dir, expected string) (bool, error) {
	var selected string
	var seen, jsonMode bool
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() string {
			if at := strings.IndexByte(arg, '='); at >= 0 {
				return arg[at+1:]
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case arg == "--continue" || arg == "-c" || strings.HasPrefix(arg, "--continue=") || arg == "--fork" || strings.HasPrefix(arg, "--fork="):
			return false, fmt.Errorf("unsafe session selection %q: use the review's explicit session ID", arg)
		case arg == "--attach" || strings.HasPrefix(arg, "--attach=") || arg == "--server" || strings.HasPrefix(arg, "--server="):
			return false, errors.New("remote server arguments cannot be used with local session ownership validation")
		case arg == "--session" || arg == "-s" || strings.HasPrefix(arg, "--session=") || strings.HasPrefix(arg, "-s") && !strings.HasPrefix(arg, "--"):
			if seen {
				return false, errors.New("multiple session arguments are not allowed")
			}
			seen = true
			if strings.HasPrefix(arg, "-s") && len(arg) > 2 && !strings.HasPrefix(arg, "--") {
				selected = arg[2:]
			} else {
				selected = value()
			}
		case arg == "--format" || strings.HasPrefix(arg, "--format="):
			jsonMode = value() == "json"
		case arg == "--dir" || strings.HasPrefix(arg, "--dir="):
			if !sameSessionDirectory(value(), dir) {
				return false, errors.New("tool --dir does not match the review checkout")
			}
		}
	}
	if seen && selected == "" || selected != expected {
		return false, errors.New("command session argument does not match the review's bound session ID")
	}
	return jsonMode, nil
}

// OpenCode 1.x and 2.2.5 run --format json emit JSONL with a top-level sessionID
// and filter events to the active root session before emitting them. Sources:
// v1.19.0/packages/opencode/src/cli/cmd/run.ts and
// v2.2.5/packages/cli/src/run/noninteractive.ts in anomalyco/opencode.
// Parse stdout incrementally so an early ID is retained even if the bounded
// transcript truncates, and never recursively inspect part.text/tool output.
type sessionStream struct {
	pending             []byte
	sessionID, expected string
	err                 error
	toolErr             error
	write               func(string)
	cancel              context.CancelFunc
}

const maxSessionEventBytes = 8 * 1024 * 1024

func (s *sessionStream) fail(err error) {
	if s.err == nil {
		s.err = err
		s.cancel()
	}
}

func (s *sessionStream) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && s.err == nil {
		end := bytes.IndexByte(p, '\n')
		chunk := p
		if end >= 0 {
			chunk = p[:end+1]
		}
		if len(s.pending)+len(chunk) > maxSessionEventBytes {
			s.pending = nil
			s.fail(errors.New("review JSON event exceeds 8 MiB"))
			break
		}
		s.pending = append(s.pending, chunk...)
		p = p[len(chunk):]
		if end >= 0 {
			s.line(s.pending)
			s.pending = s.pending[:0]
		}
	}
	return n, nil
}

func (s *sessionStream) finish() {
	if len(s.pending) > 0 && s.err == nil {
		s.line(s.pending)
	}
	s.pending = nil
}

func (s *sessionStream) line(line []byte) {
	var event struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionID"`
		Part      struct {
			SessionID string                         `json:"sessionID"`
			Text      string                         `json:"text"`
			Tool      string                         `json:"tool"`
			State     struct{ Status, Error string } `json:"state"`
		} `json:"part"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Name    string `json:"name"`
			Data    struct {
				Message string `json:"message"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &event); err != nil {
		s.write(string(line))
		return
	}
	switch event.Type {
	case "step_start", "step_finish", "text", "reasoning", "tool_use", "error":
	default:
		s.write(string(line))
		return
	}
	// V2 can emit an error before session creation with an empty sessionID.
	// Preserve its diagnostic, but never treat it as a session binding.
	if event.SessionID == "" && event.Type != "error" || event.Part.SessionID != "" && event.Part.SessionID != event.SessionID {
		s.fail(errors.New("invalid root session identity in tool JSON event"))
		return
	}
	if event.SessionID != "" && (s.expected != "" && event.SessionID != s.expected || s.sessionID != "" && event.SessionID != s.sessionID) {
		s.fail(errors.New("tool emitted a different session ID during this review"))
		return
	}
	if event.SessionID != "" {
		s.sessionID = event.SessionID
	}
	switch event.Type {
	case "text", "reasoning":
		if event.Part.Text != "" {
			s.write(event.Part.Text + "\n")
		}
	case "tool_use":
		s.write(fmt.Sprintf("[tool %s: %s]\n", event.Part.Tool, event.Part.State.Status))
		if event.Part.State.Error != "" {
			s.write(event.Part.State.Error + "\n")
		}
	case "error":
		message := event.Error.Message
		if message == "" {
			message = event.Error.Data.Message
		}
		if message == "" {
			message = event.Error.Name
		}
		if message == "" {
			message = event.Error.Type
		}
		s.write(message + "\n")
		s.toolErr = errors.Join(s.toolErr, fmt.Errorf("review tool error: %s", message))
	}
}
