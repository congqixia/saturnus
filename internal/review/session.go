package review

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"
)

func (s *Service) captureSessionID(repoRoot, reviewID string) string {
	if s.cfg.Tool == "" {
		return ""
	}
	title := "saturnus-review-" + reviewID
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.cfg.Tool, "session", "list", "--format", "json", "-n", "10")
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
