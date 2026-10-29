package review

import (
	"errors"
	"net/url"
	"strings"

	"saturnus/internal/store"
)

var ErrStaleResult = errors.New("review result does not match the current run")

func (s *Service) resultCallbackURL(reviewID, runID string) string {
	if s.cfg.ResultAPIURL == "" {
		return ""
	}
	raw := strings.ReplaceAll(s.cfg.ResultAPIURL, "{review_id}", reviewID)
	u, err := url.Parse(raw)
	if err != nil || runID == "" {
		return raw
	}
	q := u.Query()
	q.Set("run_id", runID)
	u.RawQuery = q.Encode()
	return u.String()
}

// completionResult is called under syncLocks, shared with result ingestion,
// retry and reset. Process exit zero is insufficient when a structured result
// was requested; a previous round's verdict must not satisfy this run.
func (s *Service) completionResult(req *store.ReviewRequest, status, output, errText string) (string, string) {
	run, runErr := s.store.CurrentRun(req.ID)
	if fresh, err := s.store.GetReview(req.ID); err == nil {
		req.Verdict, req.Summary = fresh.Verdict, fresh.Summary
	}
	if status != "succeeded" || s.cfg.ResultAPIURL == "" {
		return status, errText
	}
	if runErr == nil && run.Verdict != "" && run.Summary != "" {
		return status, errText
	}
	reason := "missing structured review result: tool exited successfully but this run did not submit a verdict, summary and findings"
	if strings.Contains(output, "auto-rejecting") || strings.Contains(output, "The user rejected permission") {
		reason += "; tool permission was rejected; check the execution host's OpenCode permissions"
	} else if strings.Contains(output, "read-only file system") || strings.Contains(output, "Permission denied") || strings.Contains(output, "permission denied") {
		reason += "; filesystem access failed; check checkout and cache permissions on the execution host"
	}
	return "failed", reason
}
