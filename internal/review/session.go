package review

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"
)

// CaptureSessionID finds the tool session created for a review by its title,
// falling back to the most recently updated session in the checkout. It is
// used both by the server (local execution) and by remote workers to record the
// session a retry should resume.
func CaptureSessionID(ctx context.Context, tool, repoRoot, reviewID string) string {
	if tool == "" {
		return ""
	}
	title := "saturnus-review-" + reviewID
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

func (s *Service) captureSessionID(repoRoot, reviewID string) string {
	if s.cfg.Tool == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return CaptureSessionID(ctx, s.cfg.Tool, repoRoot, reviewID)
}
