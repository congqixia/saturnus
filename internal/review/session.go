package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CaptureSessionID is legacy discovery for a local action on a review without
// a saved ID. New executions bind IDs from their own stdout in RunWithSession.
// Discovery requires one unambiguous root with the exact title and checkout.
func CaptureSessionID(ctx context.Context, tool, repoRoot, reviewID string) string {
	return CaptureContextSessionID(ctx, tool, repoRoot, reviewID, "")
}

// SessionTitle isolates reset contexts while preserving legacy titles.
func SessionTitle(reviewID, contextID string) string {
	title := "saturnus-review-" + reviewID
	if contextID != "" {
		title += "-" + contextID
	}
	return title
}

// CaptureContextSessionID never falls back to another context or directory
// after a reset, even if the tool failed before creating its new session.
func CaptureContextSessionID(ctx context.Context, tool, repoRoot, reviewID, contextID string) string {
	if tool == "" {
		return ""
	}
	title := SessionTitle(reviewID, contextID)
	cmd := exec.CommandContext(ctx, tool, "session", "list", "--format", "json")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	var sessions []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Directory string `json:"directory"`
		ParentID  string `json:"parentID"`
	}
	if err := json.Unmarshal(out, &sessions); err != nil {
		return ""
	}
	var matchedID string
	for _, sess := range sessions {
		if sess.Title == title && sess.ID != "" && sess.ParentID == "" && sameSessionDirectory(sess.Directory, repoRoot) {
			// Ambiguous ownership must never be resolved by recency.
			if matchedID != "" && matchedID != sess.ID {
				return ""
			}
			matchedID = sess.ID
		}
	}
	return matchedID
}

// ValidateSession looks up an explicit ID, never a recent-session candidate.
// OpenCode 1.x uses export <id> with info.directory; 2.x uses session export
// <id> with info.location.directory. Both return {info:...,messages:[...]}.
// Decode only info and drain the transcript without buffering or logging it.
// This check must run on the host that will execute --session.
func ValidateSession(ctx context.Context, tool, repoRoot, reviewID, contextID, sessionID string) error {
	if sessionID == "" {
		return errors.New("missing session ID")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	version := exec.CommandContext(checkCtx, tool, "--version")
	version.Dir = repoRoot
	setupProcessGroup(version)
	output, err := version.Output()
	if err != nil {
		return fmt.Errorf("detect session protocol: %w", err)
	}
	major, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(strings.TrimSpace(string(output)), "v"), ".", 2)[0])
	var args []string
	switch {
	case err == nil && major == 1:
		args = []string{"export", sessionID}
	case err == nil && major == 2:
		args = []string{"session", "export", sessionID}
	default:
		return errors.New("cannot verify session: tool must report a supported OpenCode 1.x or 2.x version")
	}
	cmd := exec.CommandContext(checkCtx, tool, args...)
	cmd.Dir = repoRoot
	setupProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("read session metadata: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("read session metadata: %w", err)
	}
	info, readErr := readSessionMetadata(json.NewDecoder(stdout))
	if readErr != nil {
		cancel()
	}
	_, drainErr := io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	if err := errors.Join(readErr, drainErr, waitErr); err != nil {
		return fmt.Errorf("cannot verify session %s; reset the review context before retrying: %w", sessionID, err)
	}
	dir := info.Directory
	if major == 2 {
		dir = info.Location.Directory
	}
	if info.ID != sessionID || info.ParentID != "" || info.Title != SessionTitle(reviewID, contextID) || !sameSessionDirectory(dir, repoRoot) {
		return fmt.Errorf("session %s does not belong to review %s in this checkout/context; reset the review context before retrying", sessionID, reviewID)
	}
	return nil
}

type sessionMetadata struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	ParentID  string `json:"parentID"`
	Location  struct {
		Directory string `json:"directory"`
	} `json:"location"`
}

func readSessionMetadata(dec *json.Decoder) (sessionMetadata, error) {
	var info sessionMetadata
	token, err := dec.Token()
	if err != nil {
		return info, err
	}
	if token != json.Delim('{') {
		return info, errors.New("invalid session metadata export")
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return info, err
		}
		if key == "info" {
			err := dec.Decode(&info)
			return info, err
		}
		// OpenCode writes info first. Do not buffer a transcript from an
		// incompatible export format while attempting to infer ownership.
		return info, errors.New("session export is missing leading info metadata")
	}
	return info, errors.New("session export is missing info metadata")
}

func sameSessionDirectory(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	canonical := func(path string) string {
		abs, err := filepath.Abs(path)
		if err != nil {
			return filepath.Clean(path)
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			return resolved
		}
		return abs
	}
	return canonical(a) == canonical(b)
}

func (s *Service) captureSessionID(repoRoot, reviewID string) string {
	if s.cfg.Tool == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	contextID, err := s.ContextID(reviewID)
	if err != nil {
		logf("read session context for review %s: %v", reviewID, err)
		return ""
	}
	return CaptureContextSessionID(ctx, s.cfg.Tool, repoRoot, reviewID, contextID)
}
