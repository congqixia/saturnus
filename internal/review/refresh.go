package review

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"saturnus/internal/lark"
	"saturnus/internal/store"
)

var ErrSyncDeferred = errors.New("review synchronization deferred while record is changing or running")

// SyncPR is the server-internal refresh capability. It never queues a review and
// uses already fetched GitHub metadata; sender checks remain on Refresh.
func (s *Service) SyncPR(ctx context.Context, repo string, num int, info PRInfo) error {
	if !s.cfg.Enabled {
		return nil
	}
	req, err := s.store.GetReviewByPR(repo, num)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, result, err := s.syncReview(ctx, req.ID, info)
	if err != nil {
		return err
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	return nil
}

func (s *Service) syncReview(ctx context.Context, id string, info PRInfo) (store.ReviewRequest, RefreshResult, error) {
	result := RefreshResult{PRState: info.State}
	unlock, err := s.syncLocks.Lock(ctx, id)
	if err != nil {
		return store.ReviewRequest{}, result, err
	}
	defer unlock()
	req, err := s.store.GetReview(id)
	if err != nil {
		return req, result, err
	}
	if req.Status == "pending" || req.Status == "reviewing" {
		return req, result, ErrSyncDeferred
	}
	if info.State != "OPEN" && info.State != "MERGED" && info.State != "CLOSED" {
		result.Error = "cannot resolve PR state (is gh configured?)"
		return req, result, nil
	}
	before := req
	if info.HeadCommit != "" {
		req.PrHeadCommit = info.HeadCommit
	}
	if info.Title != "" {
		req.Title = info.Title
	}
	if info.BaseBranch != "" {
		req.BaseBranch = info.BaseBranch
	}
	switch info.State {
	case "MERGED", "CLOSED":
		if req.Status != "archived" {
			req.Status = "archived"
			req.ArchivedAt = time.Now().UTC()
			result.Archived = true
		}
		if req.TaskID != "" && s.lark.Enabled() {
			task, err := s.lark.GetTaskContext(ctx, req.TaskID)
			if err != nil {
				result.Error = "load task: " + err.Error()
			} else if !task.Completed {
				if err := s.lark.UpdateTaskContext(ctx, req.TaskID, lark.UpdateTaskInput{Completed: boolPtr(true)}); err != nil {
					result.Error = "complete task: " + err.Error()
				} else {
					result.TaskCompleted = true
				}
			}
		}
	case "OPEN":
		if req.Status == "archived" {
			req.Status = "succeeded"
			req.ArchivedAt = time.Time{}
			result.Unarchived = true
		}
		if req.TaskID != "" && s.lark.Enabled() {
			s.syncReviewsToTask(&req, &result, ctx)
		}
	}
	if result.Error == "" {
		req.RefreshedAt = time.Now().UTC()
	}
	// Checkpoint local state after a cancelled remote call; shutdown waits for it.
	checkpoint, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	saved, err := s.store.SaveReviewSync(checkpoint, before, req)
	if err != nil {
		return req, result, err
	}
	if !saved {
		return req, result, ErrSyncDeferred
	}
	req, err = s.store.GetReview(id)
	return req, result, err
}

// reviewMarker survives a remote-success/local-save-failure retry. Legacy fake
// payloads without IDs use a deterministic content digest.
func reviewMarker(r PRReview) string {
	if r.ID != 0 {
		return fmt.Sprintf("[github-review:%d]", r.ID)
	}
	digest := sha256.Sum256([]byte(r.User + "\x00" + r.State + "\x00" + r.SubmittedAt + "\x00" + r.Body))
	return fmt.Sprintf("[github-review:%x]", digest[:12])
}

func (s *Service) syncReviewsToTask(req *store.ReviewRequest, result *RefreshResult, ctx context.Context) {
	reviews, err := FetchPRReviews(ctx, s.cfg.GHCli, req.Repo, req.PRNumber)
	if err != nil {
		result.Error = "PR reviews: " + err.Error()
		return
	}
	// IDs break timestamp ties; include the watermark timestamp to avoid losing
	// reviews submitted within the same second after a partial append.
	for _, r := range reviews {
		if _, err := time.Parse(time.RFC3339, r.SubmittedAt); err != nil {
			result.Error = "PR review has invalid submitted_at"
			return
		}
	}
	sort.SliceStable(reviews, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339, reviews[i].SubmittedAt)
		b, _ := time.Parse(time.RFC3339, reviews[j].SubmittedAt)
		if a.Equal(b) {
			return reviews[i].ID < reviews[j].ID
		}
		return a.Before(b)
	})
	var pending []PRReview
	for _, r := range reviews {
		t, err := time.Parse(time.RFC3339, r.SubmittedAt)
		if err != nil {
			result.Error = "PR review has invalid submitted_at"
			return
		}
		if !t.Before(req.TaskSyncedAt) {
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		return
	}
	task, err := s.lark.GetTaskContext(ctx, req.TaskID)
	if err != nil {
		result.Error = "load task: " + err.Error()
		return
	}
	desc := task.Description
	newest := req.TaskSyncedAt
	for _, r := range pending {
		t, _ := time.Parse(time.RFC3339, r.SubmittedAt)
		marker := reviewMarker(r)
		if !strings.Contains(desc, marker) {
			addition := fmt.Sprintf("\n\n%s [review] %s (%s) %s\n%s", marker, r.User, r.State, r.SubmittedAt, truncate(strings.TrimSpace(r.Body), 500))
			if utf8.RuneCountInString(desc+addition) > 3000 {
				result.Error = "task description is full; review synchronization will retry"
				break
			}
			desc += addition
			result.NewReviews++
		}
		if t.After(newest) {
			newest = t
		}
	}
	if desc != task.Description {
		desc = strings.TrimSpace(desc)
		if err := s.lark.UpdateTaskContext(ctx, req.TaskID, lark.UpdateTaskInput{Description: &desc}); err != nil {
			result.Error = joinErrors(result.Error, "update task: "+err.Error())
			result.NewReviews = 0
			return
		}
		result.TaskUpdated = true
	}
	req.TaskSyncedAt = newest
}
