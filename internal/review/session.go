package review

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"time"
)

// CaptureSessionID finds the tool session created for a review by its title,
// falling back to the most recently updated session in the checkout. It is
// used both by the server (local execution) and by remote workers to record the
// session a retry should resume.
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
	cmd := exec.CommandContext(ctx, tool, "session", "list", "--format", "json", "-n", "10")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	var sessions []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Directory string `json:"directory"`
		Updated   int64  `json:"updated"`
	}
	if err := json.Unmarshal(out, &sessions); err != nil {
		return ""
	}
	if contextID != "" {
		bestID := ""
		var updated int64
		for _, sess := range sessions {
			if sess.Title == title && sess.ID != "" && sameSessionDirectory(sess.Directory, repoRoot) && (bestID == "" || sess.Updated > updated) {
				bestID, updated = sess.ID, sess.Updated
			}
		}
		return bestID
	}
	for _, sess := range sessions {
		if sess.Title == title && sess.ID != "" {
			return sess.ID
		}
	}
	var bestID string
	var bestUpdated int64
	for _, sess := range sessions {
		if sess.Directory == repoRoot && sess.Updated > bestUpdated {
			bestID, bestUpdated = sess.ID, sess.Updated
		}
	}
	return bestID
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
