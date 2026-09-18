package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"saturnus/internal/lark"
	"saturnus/internal/store"
)

type Config struct {
	Enabled         bool
	AllowedUsers    []string
	ReviewerOpenID  string
	ReviewerChatID  string
	ReplyInThread   bool
	Repos           map[string]string
	Tool            string
	CommandTemplate string
	MaxConcurrent   int
	Timeout         time.Duration
	CreateTask      bool
	TaskDueHours    int
	GHCli           string
	LogDir          string
	Guides          map[string]string
}

var ErrAlreadyPending = errors.New("review already in progress")

// ErrReviewUnchanged is returned when re-requesting a review whose PR head
// commit has not changed since the last (successful) review.
var ErrReviewUnchanged = errors.New("review is already up to date")

// ErrArchived is returned when a review was archived (its PR was merged or
// closed) and a re-run is attempted.
var ErrArchived = errors.New("review is archived")

type MessageContext struct {
	Text          string
	ChatID        string
	SenderOpenID  string
	SenderName    string
	MessageID     string
	RootMessageID string
	Topic         string
}

type larkClient interface {
	Enabled() bool
	GetUserName(openID string) (string, error)
	SendText(receiveIDType, receiveID, text string) error
	SendTextToUser(openID, text string) error
	CreateTask(input lark.CreateTaskInput) (lark.Task, error)
	GetTask(taskGUID string) (lark.Task, error)
	UpdateTask(taskGUID string, input lark.UpdateTaskInput) error
	ReplyMessage(messageID, text string) error
}

// job is one unit of work for the worker pool: a review execution or a
// reviewer-triggered reaction on a finished review.
type job struct {
	kind string // "review" or "reaction"
	id   string
}

type Service struct {
	store  *store.Store
	lark   larkClient
	cfg    Config
	jobs   chan job
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc

	// nameFailures tracks recent requester-name resolution failures by open_id
	// so repeated lookups (e.g. web UI polling) do not hammer the contact API
	// or spam the logs. Keyed by open_id, value is the failure time.
	nameFailures   map[string]time.Time
	nameFailuresMu sync.Mutex
}

func New(st *store.Store, lk larkClient, cfg Config) *Service {
	if cfg.Tool == "" {
		cfg.Tool = "opencode"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Minute
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.TaskDueHours <= 0 {
		cfg.TaskDueHours = 24
	}
	if cfg.CommandTemplate == "" {
		cfg.CommandTemplate = DefaultCommandTemplate
	}
	if cfg.GHCli == "" {
		cfg.GHCli = "gh"
	}
	if cfg.LogDir == "" {
		cfg.LogDir = os.TempDir() + "/saturnus-review-logs"
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		store:        st,
		lark:         lk,
		cfg:          cfg,
		jobs:         make(chan job, 64),
		ctx:          ctx,
		cancel:       cancel,
		nameFailures: map[string]time.Time{},
	}
}

func ParseRepoMap(value string) map[string]string {
	out := map[string]string{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			continue
		}
		repo := strings.TrimSpace(parts[0])
		dir := strings.TrimSpace(parts[1])
		if repo != "" && dir != "" {
			out[repo] = dir
		}
	}
	return out
}

func (s *Service) Enabled() bool {
	return s.cfg.Enabled
}

// Tool returns the configured review tool name (e.g. "opencode").
func (s *Service) Tool() string {
	return s.cfg.Tool
}

func (s *Service) Start() {
	if !s.cfg.Enabled {
		return
	}
	if _, err := s.store.FailInterruptedReviews(); err != nil {
		logf("fail interrupted reviews: %v", err)
	}
	if _, err := s.store.FailInterruptedReactions(); err != nil {
		logf("fail interrupted reactions: %v", err)
	}
	s.BackfillRequesterNames()
	for i := 0; i < s.cfg.MaxConcurrent; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	for _, req := range s.store.ListReviews("pending", false) {
		s.enqueue("review", req.ID)
	}
	for _, reac := range s.store.ListPendingReactions() {
		s.enqueue("reaction", reac.ID)
	}
}

func (s *Service) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.jobs != nil {
		close(s.jobs)
	}
	s.wg.Wait()
}

func (s *Service) worker() {
	defer s.wg.Done()
	for j := range s.jobs {
		switch j.kind {
		case "reaction":
			s.processReaction(j.id)
		default:
			s.process(j.id)
		}
	}
}

func (s *Service) enqueue(kind, id string) {
	j := job{kind: kind, id: id}
	select {
	case s.jobs <- j:
	default:
		go func() { s.jobs <- j }()
	}
}

func (s *Service) Submit(ctx MessageContext) (store.ReviewRequest, error) {
	return s.submit(ctx, true)
}

func (s *Service) SubmitPublic(ctx MessageContext) (store.ReviewRequest, error) {
	return s.submit(ctx, false)
}

func (s *Service) submit(ctx MessageContext, requireUser bool) (store.ReviewRequest, error) {
	if !s.cfg.Enabled {
		return store.ReviewRequest{}, errors.New("PR review is disabled")
	}
	if requireUser && !s.allowed(ctx.SenderOpenID) {
		return store.ReviewRequest{}, errors.New("sender not allowed to request PR review")
	}
	pr, err := FindPRRequest(ctx.Text)
	if err != nil {
		return store.ReviewRequest{}, err
	}
	if _, ok := s.repoDirFor(pr.Repo); !ok {
		return store.ReviewRequest{}, fmt.Errorf("repo %s is not in the review whitelist (SATURNUS_REVIEW_REPOS)", pr.Repo)
	}
	thread, err := s.store.GetOrCreateReviewThread(ctx.ChatID, ctx.RootMessageID, ctx.Topic, []string{ctx.SenderOpenID})
	if err != nil {
		return store.ReviewRequest{}, err
	}
	requesterName := s.resolveName(ctx)

	existing, err := s.store.GetReviewByPR(pr.Repo, pr.PRNumber)
	if err == nil {
		switch existing.Status {
		case "pending", "reviewing":
			return existing, ErrAlreadyPending
		case "archived":
			return existing, ErrArchived
		}
		var reason string
		if existing.Status == "succeeded" {
			// A successful review is only re-run when the PR head commit moved;
			// otherwise the result is already current for that commit.
			changed, head := s.headChanged(&existing)
			if !changed {
				return existing, ErrReviewUnchanged
			}
			reason = "re-submitted; PR head changed"
			if head != "" {
				reason += " to " + head
			}
		} else {
			reason = "re-submitted after failure"
			if existing.Error != "" {
				reason += ": " + existing.Error
			}
		}
		return s.retryFinished(&existing, ctx, requesterName, reason)
	}

	req := store.ReviewRequest{
		ThreadID:        thread.ID,
		Status:          "pending",
		PRURL:           pr.PRURL,
		Repo:            pr.Repo,
		PRNumber:        pr.PRNumber,
		RequesterOpenID: ctx.SenderOpenID,
		RequesterName:   requesterName,
		ChatID:          ctx.ChatID,
		MessageID:       ctx.MessageID,
		Tool:            s.cfg.Tool,
	}
	req, err = s.store.CreateReview(req)
	if err != nil {
		return store.ReviewRequest{}, err
	}
	if err := s.store.LinkReviewThread(req.ID, req.ThreadID); err != nil {
		logf("link review thread %s: %v", req.ID, err)
	}
	s.maybeCreateTask(&req)
	s.enqueue("review", req.ID)
	return req, nil
}

// Retry resets a finished review and queues another run. Failed reviews are
// always retried; successful reviews are retried only when the PR head commit
// moved since the recorded one. The previous tool session (if any) is kept so
// the new run continues the existing review conversation.
func (s *Service) Retry(id string, ctx MessageContext) (store.ReviewRequest, error) {
	if !s.cfg.Enabled {
		return store.ReviewRequest{}, errors.New("PR review is disabled")
	}
	if !s.allowed(ctx.SenderOpenID) {
		return store.ReviewRequest{}, errors.New("sender not allowed to retry a review")
	}
	req, err := s.store.GetReview(id)
	if err != nil {
		return store.ReviewRequest{}, err
	}
	switch req.Status {
	case "pending", "reviewing":
		return req, ErrAlreadyPending
	case "archived":
		return req, ErrArchived
	}
	var reason string
	if req.Status == "succeeded" {
		changed, head := s.headChanged(&req)
		if !changed {
			return req, ErrReviewUnchanged
		}
		reason = "retry; PR head changed"
		if head != "" {
			reason += " to " + head
		}
	} else {
		reason = "retry after failure"
		if req.Error != "" {
			reason += ": " + req.Error
		}
	}
	return s.retryFinished(&req, ctx, s.resolveName(ctx), reason)
}

// retryFinished resets a finished review to pending and re-queues it. The
// existing SessionID and RetryReason are kept so process() can resume the same
// tool conversation and explain why the run is happening.
//
// The requester identity is preserved across re-runs: a re-submission or retry
// by anyone else (typically the reviewer continuing the review) must not
// reassign who receives requester replies and task notifications. Only when the
// recorded requester is a placeholder (empty, or the HTTP-API "api" sentinel)
// is the new sender adopted as the requester. The chat/message/thread context
// is still updated so the latest request is recorded in the review thread.
func (s *Service) retryFinished(existing *store.ReviewRequest, ctx MessageContext, requesterName, reason string) (store.ReviewRequest, error) {
	existing.Status = "pending"
	existing.ResultText, existing.Error = "", ""
	existing.CompletedAt = time.Time{}
	existing.RetryReason = reason
	if isPlaceholderRequester(existing.RequesterOpenID) {
		existing.RequesterOpenID = ctx.SenderOpenID
		existing.RequesterName = requesterName
	}
	existing.ChatID = ctx.ChatID
	existing.MessageID = ctx.MessageID
	if ctx.ChatID != "" || ctx.RootMessageID != "" {
		thread, err := s.store.GetOrCreateReviewThread(ctx.ChatID, ctx.RootMessageID, ctx.Topic, []string{ctx.SenderOpenID})
		if err != nil {
			return store.ReviewRequest{}, err
		}
		existing.ThreadID = thread.ID
	}
	if _, err := s.store.UpdateReview(*existing); err != nil {
		return store.ReviewRequest{}, err
	}
	if existing.ThreadID != "" {
		if err := s.store.LinkReviewThread(existing.ID, existing.ThreadID); err != nil {
			logf("link review thread %s: %v", existing.ID, err)
		}
	}
	s.maybeCreateTask(existing)
	s.enqueue("review", existing.ID)
	return *existing, nil
}

// isPlaceholderRequester reports whether a recorded requester open_id is a real
// person. Empty values and the "api" sentinel (used by POST /api/reviews when
// no requester is supplied) are placeholders that a real sender may replace on
// a later re-run.
func isPlaceholderRequester(openID string) bool {
	return openID == "" || openID == "api"
}

// RefreshResult reports what a /sat refresh changed for one review.
type RefreshResult struct {
	PRState       string
	Archived      bool
	Unarchived    bool
	TaskUpdated   bool
	TaskCompleted bool
	NewReviews    int
	Error         string
}

// Refresh re-syncs a review with its PR and Feishu task:
//   - if the PR was merged or closed the review is archived (hidden from
//     default listings) and its tracking task is completed;
//   - if a previously archived PR was reopened the review is restored;
//   - any new reviewer reviews on the PR are appended to the task description
//     (tracked via TaskSyncedAt so each comment is synced once).
func (s *Service) Refresh(id string, ctx MessageContext) (store.ReviewRequest, RefreshResult, error) {
	var res RefreshResult
	if !s.cfg.Enabled {
		return store.ReviewRequest{}, res, errors.New("PR review is disabled")
	}
	if !s.allowed(ctx.SenderOpenID) {
		return store.ReviewRequest{}, res, errors.New("sender not allowed to refresh a review")
	}
	req, err := s.store.GetReview(id)
	if err != nil {
		return store.ReviewRequest{}, res, err
	}

	ctx2, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info := ResolvePRInfo(ctx2, s.cfg.GHCli, req.Repo, req.PRNumber)
	res.PRState = info.State
	if res.PRState == "" {
		res.Error = "cannot resolve PR state (is gh configured?)"
	}

	now := time.Now().UTC()
	req.RefreshedAt = now

	switch res.PRState {
	case "MERGED", "CLOSED":
		if req.Status != "archived" {
			req.Status = "archived"
			req.ArchivedAt = now
			res.Archived = true
		}
		// Complete the task only on the archive transition: the task API
		// rejects completing an already-completed task.
		if res.Archived && req.TaskID != "" && s.lark.Enabled() {
			if err := s.lark.UpdateTask(req.TaskID, lark.UpdateTaskInput{Completed: boolPtr(true)}); err != nil {
				res.Error = joinErrors(res.Error, "complete task: "+err.Error())
			} else {
				res.TaskCompleted = true
			}
		}
	case "OPEN":
		if req.Status == "archived" {
			req.Status = "succeeded"
			req.ArchivedAt = time.Time{}
			res.Unarchived = true
		}
		if req.TaskID != "" && s.lark.Enabled() {
			s.syncReviewsToTask(&req, &res, ctx2)
		}
	}

	if _, err := s.store.UpdateReview(req); err != nil {
		return store.ReviewRequest{}, res, err
	}
	return req, res, nil
}

// RefreshAll refreshes every non-archived review and returns one result per
// review. Errors are collected per review instead of aborting the sweep.
func (s *Service) RefreshAll(ctx MessageContext) ([]store.ReviewRequest, []RefreshResult, error) {
	reqs := s.store.ListReviews("", false)
	out := make([]store.ReviewRequest, 0, len(reqs))
	results := make([]RefreshResult, 0, len(reqs))
	for _, req := range reqs {
		updated, res, err := s.Refresh(req.ID, ctx)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, updated)
		results = append(results, res)
	}
	return out, results, nil
}

// syncReviewsToTask appends reviewer reviews newer than TaskSyncedAt onto the
// review's Feishu task description and advances the sync marker.
func (s *Service) syncReviewsToTask(req *store.ReviewRequest, res *RefreshResult, ctx context.Context) {
	reviews := ResolvePRReviews(ctx, s.cfg.GHCli, req.Repo, req.PRNumber)
	var newOnes []PRReview
	var newest time.Time
	for _, r := range reviews {
		t, err := time.Parse(time.RFC3339, r.SubmittedAt)
		if err != nil {
			continue
		}
		if t.After(req.TaskSyncedAt) {
			newOnes = append(newOnes, r)
			if t.After(newest) {
				newest = t
			}
		}
	}
	if len(newOnes) == 0 {
		return
	}
	task, err := s.lark.GetTask(req.TaskID)
	if err != nil {
		res.Error = joinErrors(res.Error, "load task: "+err.Error())
		return
	}
	desc := task.Description
	for _, r := range newOnes {
		desc += fmt.Sprintf("\n\n[review] %s (%s) %s\n%s", r.User, r.State, r.SubmittedAt, truncate(strings.TrimSpace(r.Body), 500))
	}
	desc = strings.TrimSpace(desc)
	if err := s.lark.UpdateTask(req.TaskID, lark.UpdateTaskInput{Description: &desc}); err != nil {
		res.Error = joinErrors(res.Error, "update task: "+err.Error())
		return
	}
	res.NewReviews = len(newOnes)
	res.TaskUpdated = true
	req.TaskSyncedAt = newest
}

// FormatRefreshResult renders the outcome of a refresh for the command reply.
func (s *Service) FormatRefreshResult(req store.ReviewRequest, res RefreshResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review %s refreshed\n%s\nrepo=%s#%d pr_state=%s", req.ID, req.PRURL, req.Repo, req.PRNumber, res.PRState)
	switch {
	case res.Archived:
		b.WriteString("\nstatus=archived (PR merged/closed)")
	case res.Unarchived:
		b.WriteString("\nstatus=restored to succeeded (PR reopened)")
	default:
		fmt.Fprintf(&b, "\nstatus=%s", req.Status)
	}
	if res.TaskUpdated {
		fmt.Fprintf(&b, "\ntask updated: synced %d new review comment(s)", res.NewReviews)
	}
	if res.TaskCompleted {
		b.WriteString("\ntask completed")
	}
	if res.Error != "" {
		fmt.Fprintf(&b, "\nerror=%s", res.Error)
	}
	return b.String()
}

// FormatRefreshAll renders the outcome of a refresh-all sweep.
func (s *Service) FormatRefreshAll(reqs []store.ReviewRequest, results []RefreshResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Refreshed %d review(s):\n", len(reqs))
	for i, req := range reqs {
		res := results[i]
		fmt.Fprintf(&b, "- %s (%s#%d) pr_state=%s status=%s", req.ID, req.Repo, req.PRNumber, res.PRState, req.Status)
		if res.Archived {
			b.WriteString(" archived")
		}
		if res.TaskUpdated {
			fmt.Fprintf(&b, " task+%d", res.NewReviews)
		}
		if res.Error != "" {
			fmt.Fprintf(&b, " error=%s", res.Error)
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func boolPtr(v bool) *bool {
	return &v
}

// joinErrors combines two non-empty error fragments with "; ".
func joinErrors(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}

// headChanged reports whether the PR head commit moved away from the recorded
// one. Reviews without a recorded baseline are treated as changed so they can
// always be re-run. An unresolvable current head is also treated as changed to
// avoid blocking a retry.
func (s *Service) headChanged(req *store.ReviewRequest) (changed bool, head string) {
	if req.HeadCommit == "" {
		return true, ""
	}
	head = s.currentHead(req.Repo, req.PRNumber)
	if head == "" {
		logf("headChanged: cannot resolve current head for %s#%d; assuming changed", req.Repo, req.PRNumber)
		return true, ""
	}
	return head != req.HeadCommit, head
}

func (s *Service) currentHead(repo string, num int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return ResolvePRInfo(ctx, s.cfg.GHCli, repo, num).HeadCommit
}

func (s *Service) resolveName(ctx MessageContext) string {
	name := ctx.SenderName
	if name == "" && ctx.SenderOpenID != "" && s.lark.Enabled() {
		if n, err := s.lark.GetUserName(ctx.SenderOpenID); err != nil {
			logf("resolve requester name: %v", err)
		} else {
			name = n
		}
	}
	return name
}

func (s *Service) maybeCreateTask(req *store.ReviewRequest) {
	if !s.cfg.CreateTask {
		return
	}
	if req.TaskID != "" {
		return
	}
	task, err := s.createTask(*req)
	if err != nil {
		logf("create feishu task: %v", err)
		return
	}
	if task.GUID == "" {
		return
	}
	req.TaskID = task.GUID
	req.TaskURL = task.URL
	if _, err := s.store.UpdateReview(*req); err != nil {
		logf("store task id: %v", err)
	}
	s.notifyRequesterTaskCreated(*req)
}

func (s *Service) notifyRequesterTaskCreated(req store.ReviewRequest) {
	if req.RequesterOpenID == "" || !s.lark.Enabled() {
		return
	}
	var b strings.Builder
	b.WriteString("已为你的 PR 评审创建任务：\n")
	fmt.Fprintf(&b, "%s#%d\n%s\n", req.Repo, req.PRNumber, req.PRURL)
	if req.TaskURL != "" {
		fmt.Fprintf(&b, "任务链接：%s\n", req.TaskURL)
	} else if req.TaskID != "" {
		fmt.Fprintf(&b, "task_id=%s\n", req.TaskID)
	}
	b.WriteString("评审完成后评审人会收到通知，并把结果同步给你。")
	if err := s.lark.SendTextToUser(req.RequesterOpenID, b.String()); err != nil {
		logf("notify requester of task: %v", err)
	}
}

// List returns review requests, newest first. Archived reviews are excluded
// unless includeArchived is true (or status is "archived").
func (s *Service) List(status string, includeArchived bool) []store.ReviewRequest {
	return s.store.ListReviews(status, includeArchived)
}

func (s *Service) Get(id string) (store.ReviewRequest, error) {
	return s.store.GetReview(id)
}

// ListRuns returns every recorded run of a review in execution order.
func (s *Service) ListRuns(reviewID string) []store.ReviewRun {
	return s.store.ListRuns(reviewID)
}

func (s *Service) FormatList(reqs []store.ReviewRequest) string {
	if len(reqs) == 0 {
		return "No reviews."
	}
	var b strings.Builder
	for i, req := range reqs {
		if i >= 10 {
			break
		}
		fmt.Fprintf(&b, "%s %s %s#%d %s\n", req.ID, req.Status, req.Repo, req.PRNumber, req.CreatedAt.Format(time.RFC3339))
	}
	return strings.TrimSpace(b.String())
}

func (s *Service) FormatOne(req store.ReviewRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s status=%s\n%s\nrepo=%s#%d", req.ID, req.Status, req.PRURL, req.Repo, req.PRNumber)
	if req.Title != "" {
		fmt.Fprintf(&b, "\ntitle=%s", req.Title)
	}
	if req.TaskID != "" {
		fmt.Fprintf(&b, "\ntask_id=%s", req.TaskID)
		if req.TaskURL != "" {
			fmt.Fprintf(&b, "\ntask_url=%s", req.TaskURL)
		}
	}
	if req.SessionID != "" {
		fmt.Fprintf(&b, "\nsession=%s (continue: %s run --session %s)", req.SessionID, s.cfg.Tool, req.SessionID)
	}
	if req.HeadCommit != "" {
		fmt.Fprintf(&b, "\nhead=%s", req.HeadCommit)
	}
	if req.RetryReason != "" {
		fmt.Fprintf(&b, "\nretry_reason=%s", req.RetryReason)
	}
	if !req.ArchivedAt.IsZero() {
		fmt.Fprintf(&b, "\narchived_at=%s", req.ArchivedAt.Format(time.RFC3339))
	}
	if !req.RefreshedAt.IsZero() {
		fmt.Fprintf(&b, "\nrefreshed_at=%s", req.RefreshedAt.Format(time.RFC3339))
	}
	if !req.TaskSyncedAt.IsZero() {
		fmt.Fprintf(&b, "\ntask_synced_at=%s", req.TaskSyncedAt.Format(time.RFC3339))
	}
	if req.RequesterName != "" || req.RequesterOpenID != "" {
		name := req.RequesterName
		if name == "" {
			name = req.RequesterOpenID
		}
		fmt.Fprintf(&b, "\nrequester=%s", name)
	}
	if s.cfg.LogDir != "" {
		fmt.Fprintf(&b, "\nlog=%s", filepath.Join(s.cfg.LogDir, req.ID+".log"))
	}
	if req.Error != "" {
		fmt.Fprintf(&b, "\nerror=%s", req.Error)
	}
	if req.ResultText != "" {
		fmt.Fprintf(&b, "\n\n%s", truncate(stripANSI(req.ResultText), 3000))
	}
	return b.String()
}

func (s *Service) allowed(openID string) bool {
	for _, user := range s.cfg.AllowedUsers {
		if user == "*" {
			return true
		}
		if user == openID {
			return true
		}
	}
	return false
}

func (s *Service) repoDirFor(repo string) (string, bool) {
	dir, ok := s.cfg.Repos[repo]
	return dir, ok
}

func (s *Service) isReviewer(openID string) bool {
	if openID == "" {
		return false
	}
	if s.cfg.ReviewerOpenID != "" && openID == s.cfg.ReviewerOpenID {
		return true
	}
	return s.allowed(openID)
}

func (s *Service) ReplyToRequester(reviewID, senderOpenID, text string) error {
	if !s.lark.Enabled() {
		return errors.New("lark client disabled")
	}
	if !s.isReviewer(senderOpenID) {
		return errors.New("sender is not the reviewer")
	}
	req, err := s.store.GetReview(reviewID)
	if err != nil {
		return err
	}
	if req.RequesterOpenID == "" {
		return errors.New("review has no requester")
	}
	msg := fmt.Sprintf("[reply to review %s] %s\n%s", req.ID, req.PRURL, text)
	return s.lark.SendTextToUser(req.RequesterOpenID, msg)
}

// Act records a reviewer instruction for a finished review and queues it for
// execution: the configured tool resumes the review's session and carries out
// the instruction (e.g. post inline comments for selected issues, or work out
// concrete code for an issue), then the output is delivered back to the
// reviewer. The reaction is persisted so every action is traceable.
func (s *Service) Act(ctx MessageContext, reviewID, instruction string) (store.ReviewReaction, error) {
	if !s.cfg.Enabled {
		return store.ReviewReaction{}, errors.New("PR review is disabled")
	}
	if !s.isReviewer(ctx.SenderOpenID) {
		return store.ReviewReaction{}, errors.New("sender is not the reviewer")
	}
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return store.ReviewReaction{}, errors.New("empty instruction")
	}
	req, err := s.store.GetReview(reviewID)
	if err != nil {
		return store.ReviewReaction{}, err
	}
	if req.Status == "pending" || req.Status == "reviewing" {
		return store.ReviewReaction{}, errors.New("review is still in progress; wait for it to finish first")
	}
	// Resolve the tool session to resume now so a missing one fails fast
	// instead of enqueuing a job that cannot run.
	repoRoot, ok := s.repoDirFor(req.Repo)
	if !ok {
		return store.ReviewReaction{}, fmt.Errorf("repo %s is not in the review whitelist (SATURNUS_REVIEW_REPOS)", req.Repo)
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = s.captureSessionID(repoRoot, req.ID)
	}
	if sessionID == "" {
		return store.ReviewReaction{}, errors.New("review has no tool session to continue; run the review first")
	}
	reac, err := s.store.CreateReaction(store.ReviewReaction{
		ReviewID:     reviewID,
		Instruction:  instruction,
		Status:       "pending",
		ChatID:       ctx.ChatID,
		MessageID:    ctx.MessageID,
		SenderOpenID: ctx.SenderOpenID,
		SenderName:   ctx.SenderName,
		SessionID:    sessionID,
	})
	if err != nil {
		return store.ReviewReaction{}, err
	}
	s.enqueue("reaction", reac.ID)
	return reac, nil
}

// ListReactions returns every recorded reaction of a review in creation order.
func (s *Service) ListReactions(reviewID string) []store.ReviewReaction {
	return s.store.ListReactions(reviewID)
}

// ListThreads returns every thread a review has been requested in, most
// recently active first. It returns the linked thread history and always
// includes the review's current ThreadID, so reviews recorded before thread
// linking still show their thread.
func (s *Service) ListThreads(reviewID string) []store.ReviewThread {
	threads := s.store.ListReviewThreads(reviewID)
	req, err := s.store.GetReview(reviewID)
	if err != nil || req.ThreadID == "" {
		return threads
	}
	for _, t := range threads {
		if t.ID == req.ThreadID {
			return threads
		}
	}
	if current, err := s.store.GetThread(req.ThreadID); err == nil {
		threads = append(threads, current)
	}
	return threads
}

// FormatReaction renders a reaction's outcome for delivery to the reviewer.
func (s *Service) FormatReaction(reac store.ReviewReaction, req store.ReviewRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Reaction %s status=%s\n", reac.ID, reac.Status)
	fmt.Fprintf(&b, "review=%s\n%s\nrepo=%s#%d", req.ID, req.PRURL, req.Repo, req.PRNumber)
	fmt.Fprintf(&b, "\ninstruction=%s", reac.Instruction)
	if reac.Error != "" {
		fmt.Fprintf(&b, "\nerror=%s", reac.Error)
	}
	clean := stripANSI(reac.ResultText)
	if clean != "" {
		fmt.Fprintf(&b, "\n\n%s", truncate(clean, 3000))
	}
	if s.cfg.LogDir != "" {
		fmt.Fprintf(&b, "\n\nlog=%s", filepath.Join(s.cfg.LogDir, reac.ID+".log"))
	}
	return b.String()
}

func (s *Service) BackfillRequesterNames() {
	if !s.lark.Enabled() {
		return
	}
	for _, r := range s.store.ListReviews("", true) {
		if r.RequesterName == "" && r.RequesterOpenID != "" {
			if err := s.resolveRequesterName(&r); err != nil {
				logf("backfill requester names: %v", err)
			}
		}
	}
}

// ResolveRequesterName resolves and persists the requester's display name for a
// review. The error is returned so callers can log it for later analysis;
// failures are best-effort and never block review processing.
func (s *Service) ResolveRequesterName(req *store.ReviewRequest) error {
	return s.resolveRequesterName(req)
}

func (s *Service) loadGuide(repo, repoRoot string) (string, error) {
	if path, ok := s.cfg.Guides[repo]; ok && path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read guide %s: %w", path, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	candidates := []string{".saturnus-review.md", ".saturnus/review.md"}
	for _, name := range candidates {
		path := filepath.Join(repoRoot, name)
		if data, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(data)), nil
		}
	}
	return "", nil
}

func (s *Service) FormatRepos() string {
	if len(s.cfg.Repos) == 0 {
		return "No repos whitelisted. Set SATURNUS_REVIEW_REPOS=owner/repo=/path/to/checkout,..."
	}
	var b strings.Builder
	for repo, dir := range s.cfg.Repos {
		fmt.Fprintf(&b, "%s -> %s\n", repo, dir)
	}
	return strings.TrimSpace(b.String())
}

func (s *Service) createTask(req store.ReviewRequest) (lark.Task, error) {
	if !s.lark.Enabled() {
		return lark.Task{}, nil
	}
	members := taskMembers(s.cfg.ReviewerOpenID, req.RequesterOpenID)
	if len(members) == 0 {
		return lark.Task{}, nil
	}
	requester := req.RequesterName
	if requester == "" {
		requester = req.RequesterOpenID
	}
	return s.lark.CreateTask(lark.CreateTaskInput{
		Summary:       fmt.Sprintf("Review PR %s#%d", req.Repo, req.PRNumber),
		Description:   fmt.Sprintf("PR review request %s\n%s\nrequester=%s", req.ID, req.PRURL, requester),
		Due:           time.Now().Add(time.Duration(s.cfg.TaskDueHours) * time.Hour),
		MemberOpenIDs: members,
		SourceTitle:   "Saturnus PR Review",
		SourceURL:     req.PRURL,
	})
}

func taskMembers(reviewerOpenID, requesterOpenID string) []string {
	var out []string
	for _, id := range []string{reviewerOpenID, requesterOpenID} {
		if id == "" {
			continue
		}
		dup := false
		for _, existing := range out {
			if existing == id {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, id)
		}
	}
	return out
}

func (s *Service) process(id string) {
	req, err := s.store.GetReview(id)
	if err != nil {
		logf("load %s: %v", id, err)
		return
	}
	if req.Status != "pending" {
		return
	}
	req.Status = "reviewing"
	if _, err := s.store.UpdateReview(req); err != nil {
		logf("mark reviewing: %v", err)
		return
	}

	// Record this execution as an append-only run so every review attempt
	// (initial + retries) is traceable, including the head commit it reviewed.
	if _, err := s.store.CreateReviewRun(store.ReviewRun{
		ReviewID:    req.ID,
		Status:      "running",
		RetryReason: req.RetryReason,
		Tool:        s.cfg.Tool,
	}); err != nil {
		logf("create review run %s: %v", req.ID, err)
	}

	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Timeout)
	defer cancel()

	info := ResolvePRInfo(ctx, s.cfg.GHCli, req.Repo, req.PRNumber)
	if info.Title != "" && req.Title == "" {
		req.Title = info.Title
		req.BaseBranch = info.BaseBranch
	}
	// Record the PR head commit this run reviews. It is the baseline used to
	// decide whether a later retry/re-submission needs a fresh run.
	if info.HeadCommit != "" {
		req.HeadCommit = info.HeadCommit
	}
	if req.Title != "" || info.HeadCommit != "" {
		if _, err := s.store.UpdateReview(req); err != nil {
			logf("store pr info: %v", err)
		}
	}

	repoRoot, ok := s.repoDirFor(req.Repo)
	if !ok {
		s.finish(&req, "failed", "", fmt.Sprintf("repo %s not in review whitelist (SATURNUS_REVIEW_REPOS)", req.Repo))
		s.notifyReviewer(req)
		return
	}
	if !IsGitDir(repoRoot) {
		s.finish(&req, "failed", "", fmt.Sprintf("local checkout not found at %q (add it to SATURNUS_REVIEW_REPOS)", repoRoot))
		s.notifyReviewer(req)
		return
	}

	guide, err := s.loadGuide(req.Repo, repoRoot)
	if err != nil {
		s.finish(&req, "failed", "", "load review guide: "+err.Error())
		s.notifyReviewer(req)
		return
	}

	retryNote := ""
	if req.SessionID != "" {
		base := fmt.Sprintf("Continue the previous review conversation for GitHub PR %s#%d (%s) and produce the final structured review.", req.Repo, req.PRNumber, req.PRURL)
		if req.RetryReason != "" {
			retryNote = fmt.Sprintf("Reason for this run: %s. %s", req.RetryReason, base)
		} else {
			retryNote = base
		}
	}

	args, err := RenderCommand(s.cfg.CommandTemplate, ToolArgs{
		Repo:       req.Repo,
		PRNumber:   req.PRNumber,
		PRURL:      req.PRURL,
		Title:      req.Title,
		BaseBranch: req.BaseBranch,
		Worktree:   repoRoot,
		Guide:      guide,
		ReviewID:   req.ID,
		HeadCommit: req.HeadCommit,
		SessionID:  req.SessionID,
		RetryNote:  retryNote,
	})
	if err != nil {
		s.finish(&req, "failed", "", "render command: "+err.Error())
		s.notifyReviewer(req)
		return
	}

	logFile, err := openReviewLog(s.cfg.LogDir, req.ID)
	if err != nil {
		s.finish(&req, "failed", "", "prepare log: "+err.Error())
		s.notifyReviewer(req)
		return
	}
	defer logFile.Close()

	var mu sync.Mutex
	var progress string
	var flushMu sync.Mutex
	lastFlush := time.Now()
	sink := func(line string) {
		mu.Lock()
		progress += stripANSI(line)
		if len(progress) > maxResultBytes {
			progress = progress[:maxResultBytes]
		}
		_, _ = logFile.WriteString(line)
		mu.Unlock()

		flushMu.Lock()
		shouldFlush := time.Since(lastFlush) >= 5*time.Second
		if shouldFlush {
			lastFlush = time.Now()
		}
		flushMu.Unlock()
		if shouldFlush {
			mu.Lock()
			snapshot := progress
			mu.Unlock()
			req.ResultText = snapshot
			if _, err := s.store.UpdateReview(req); err != nil {
				logf("flush review progress: %v", err)
			}
		}
	}

	output, runErr := Run(ctx, s.cfg.Tool, args, repoRoot, sink)
	if sessionID := s.captureSessionID(repoRoot, req.ID); sessionID != "" {
		req.SessionID = sessionID
	}
	clean := stripANSI(output)
	req.RetryReason = ""
	if runErr != nil {
		s.finish(&req, "failed", clean, runErr.Error())
	} else {
		s.finish(&req, "succeeded", clean, "")
	}
	s.notifyReviewer(req)
}

func openReviewLog(logDir, id string) (*os.File, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(logDir, id+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(f, "== run %s started %s ==\n", id, time.Now().UTC().Format(time.RFC3339))
	return f, nil
}

func (s *Service) finish(req *store.ReviewRequest, status, result, errText string) {
	req.Status = status
	req.ResultText = result
	req.Error = errText
	req.CompletedAt = time.Now().UTC()
	if _, err := s.store.UpdateReview(*req); err != nil {
		logf("update status %s: %v", status, err)
	}
	// Record the terminal outcome on the in-flight run (head commit, session,
	// result/error) so each run's history is preserved for traceability.
	if run, err := s.store.CurrentRun(req.ID); err == nil {
		run.Status = status
		run.HeadCommit = req.HeadCommit
		run.SessionID = req.SessionID
		run.ResultText = result
		run.Error = errText
		run.CompletedAt = req.CompletedAt
		if err := s.store.UpdateReviewRun(run); err != nil {
			logf("update review run %s: %v", run.ID, err)
		}
	}
}

func (s *Service) FormatResult(req store.ReviewRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PR review %s status=%s\n%s\nrepo=%s#%d", req.ID, req.Status, req.PRURL, req.Repo, req.PRNumber)
	if req.Title != "" {
		fmt.Fprintf(&b, "\ntitle=%s", req.Title)
	}
	if req.RequesterName != "" {
		fmt.Fprintf(&b, "\nrequester=%s", req.RequesterName)
	}
	if req.TaskID != "" {
		fmt.Fprintf(&b, "\ntask_id=%s", req.TaskID)
		if req.TaskURL != "" {
			fmt.Fprintf(&b, "\ntask_url=%s", req.TaskURL)
		}
	}
	if req.SessionID != "" {
		fmt.Fprintf(&b, "\nsession=%s (continue: %s run --session %s)", req.SessionID, s.cfg.Tool, req.SessionID)
	}
	if req.HeadCommit != "" {
		fmt.Fprintf(&b, "\nhead=%s", req.HeadCommit)
	}
	if req.RetryReason != "" {
		fmt.Fprintf(&b, "\nretry_reason=%s", req.RetryReason)
	}
	if req.Error != "" {
		fmt.Fprintf(&b, "\nerror=%s", req.Error)
	}
	clean := stripANSI(req.ResultText)
	if summary := ExtractSummary(clean); summary != "" {
		if secs := parseReviewSections(summary); !secs.empty() {
			fmt.Fprintf(&b, "\n\n%s", formatReviewSections(secs))
		} else {
			fmt.Fprintf(&b, "\n\nREVIEW SUMMARY:\n%s", truncate(summary, 2000))
		}
	} else if clean != "" {
		fmt.Fprintf(&b, "\n\n%s", truncate(clean, 1500))
	}
	if s.cfg.LogDir != "" {
		fmt.Fprintf(&b, "\n\nlog=%s", filepath.Join(s.cfg.LogDir, req.ID+".log"))
	}
	return b.String()
}

// requesterNameBackoff stops repeated contact-API lookups for an open_id whose
// name resolution recently failed. Without it, the web UI's periodic review
// polling would hammer the contact API and spam the logs every few seconds.
const requesterNameBackoff = 15 * time.Minute

// resolveRequesterName resolves and persists the requester's display name for a
// review. It returns nil when the name is already set, when there is no open_id
// to look up, or on success; otherwise it returns a descriptive error for
// callers to log. Failures never block review processing. After a failure the
// open_id is backed off so retries are skipped silently for a while.
func (s *Service) resolveRequesterName(req *store.ReviewRequest) error {
	if req.RequesterName != "" || req.RequesterOpenID == "" {
		return nil
	}
	if !s.lark.Enabled() {
		return errors.New("lark client disabled")
	}
	if s.inNameBackoff(req.RequesterOpenID) {
		// Recent attempt already failed; skip silently so the caller keeps the
		// open_id fallback without another API call or log line.
		return nil
	}
	name, err := s.lark.GetUserName(req.RequesterOpenID)
	if err != nil {
		s.recordNameFailure(req.RequesterOpenID)
		return fmt.Errorf("resolve requester name for %s (open_id=%s): %w", req.ID, req.RequesterOpenID, err)
	}
	if name == "" {
		s.recordNameFailure(req.RequesterOpenID)
		return fmt.Errorf("resolve requester name for %s (open_id=%s): contact API returned no name fields (grant the app the contact:user.base:readonly scope)", req.ID, req.RequesterOpenID)
	}
	s.clearNameFailure(req.RequesterOpenID)
	req.RequesterName = name
	if _, err := s.store.UpdateReview(*req); err != nil {
		return fmt.Errorf("store requester name for %s: %w", req.ID, err)
	}
	return nil
}

func (s *Service) inNameBackoff(openID string) bool {
	s.nameFailuresMu.Lock()
	defer s.nameFailuresMu.Unlock()
	last, ok := s.nameFailures[openID]
	return ok && time.Since(last) < requesterNameBackoff
}

func (s *Service) recordNameFailure(openID string) {
	s.nameFailuresMu.Lock()
	defer s.nameFailuresMu.Unlock()
	if s.nameFailures == nil {
		s.nameFailures = map[string]time.Time{}
	}
	s.nameFailures[openID] = time.Now()
}

func (s *Service) clearNameFailure(openID string) {
	s.nameFailuresMu.Lock()
	defer s.nameFailuresMu.Unlock()
	delete(s.nameFailures, openID)
}

func (s *Service) notifyReviewer(req store.ReviewRequest) {
	if err := s.resolveRequesterName(&req); err != nil {
		logf("notifyReviewer: %v", err)
	}
	text := s.FormatResult(req)
	var err error
	switch {
	case s.cfg.ReviewerOpenID != "":
		err = s.lark.SendTextToUser(s.cfg.ReviewerOpenID, text)
	case s.cfg.ReviewerChatID != "":
		err = s.lark.SendText("chat_id", s.cfg.ReviewerChatID, text)
	}
	if err != nil {
		logf("notify reviewer: %v", err)
	}
	if s.cfg.ReplyInThread && req.MessageID != "" {
		if err := s.lark.ReplyMessage(req.MessageID, text); err != nil {
			logf("reply in thread: %v", err)
		}
	}
}

// processReaction runs one reviewer-triggered action: it resumes the review's
// tool session with the recorded instruction inside the mapped checkout, stores
// the output (streamed for progress) and delivers the result to the reviewer.
func (s *Service) processReaction(id string) {
	reac, err := s.store.GetReaction(id)
	if err != nil {
		logf("load reaction %s: %v", id, err)
		return
	}
	if reac.Status != "pending" {
		return
	}
	req, err := s.store.GetReview(reac.ReviewID)
	if err != nil {
		s.finishReaction(&reac, "failed", "", "load review: "+err.Error())
		s.notifyReaction(reac)
		return
	}
	reac.Status = "running"
	if _, err := s.store.UpdateReaction(reac); err != nil {
		logf("mark reaction running: %v", err)
		return
	}

	repoRoot, ok := s.repoDirFor(req.Repo)
	if !ok {
		s.finishReaction(&reac, "failed", "", fmt.Sprintf("repo %s not in review whitelist (SATURNUS_REVIEW_REPOS)", req.Repo))
		s.notifyReaction(reac)
		return
	}
	if !IsGitDir(repoRoot) {
		s.finishReaction(&reac, "failed", "", fmt.Sprintf("local checkout not found at %q", repoRoot))
		s.notifyReaction(reac)
		return
	}
	sessionID := reac.SessionID
	if sessionID == "" {
		sessionID = req.SessionID
	}
	if sessionID == "" {
		sessionID = s.captureSessionID(repoRoot, req.ID)
	}
	if sessionID == "" {
		s.finishReaction(&reac, "failed", "", "no tool session found for the review; the review must run first")
		s.notifyReaction(reac)
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Timeout)
	defer cancel()

	logFile, err := openReviewLog(s.cfg.LogDir, reac.ID)
	if err != nil {
		s.finishReaction(&reac, "failed", "", "prepare log: "+err.Error())
		s.notifyReaction(reac)
		return
	}
	defer logFile.Close()

	// The instruction is passed as a single argv element so arbitrary reviewer
	// text (quotes, backslashes, whitespace) survives verbatim.
	args := []string{"run", "--session", sessionID, "--title", "saturnus-review-" + req.ID, reac.Instruction}

	var mu sync.Mutex
	var progress string
	var flushMu sync.Mutex
	lastFlush := time.Now()
	sink := func(line string) {
		mu.Lock()
		progress += stripANSI(line)
		if len(progress) > maxResultBytes {
			progress = progress[:maxResultBytes]
		}
		_, _ = logFile.WriteString(line)
		mu.Unlock()

		flushMu.Lock()
		shouldFlush := time.Since(lastFlush) >= 5*time.Second
		if shouldFlush {
			lastFlush = time.Now()
		}
		flushMu.Unlock()
		if shouldFlush {
			mu.Lock()
			snapshot := progress
			mu.Unlock()
			reac.ResultText = snapshot
			if _, err := s.store.UpdateReaction(reac); err != nil {
				logf("flush reaction progress: %v", err)
			}
		}
	}

	output, runErr := Run(ctx, s.cfg.Tool, args, repoRoot, sink)
	clean := stripANSI(output)
	if runErr != nil {
		s.finishReaction(&reac, "failed", clean, runErr.Error())
	} else {
		s.finishReaction(&reac, "succeeded", clean, "")
	}
	s.notifyReaction(reac)
}

func (s *Service) finishReaction(reac *store.ReviewReaction, status, result, errText string) {
	reac.Status = status
	reac.ResultText = result
	reac.Error = errText
	reac.CompletedAt = time.Now().UTC()
	if _, err := s.store.UpdateReaction(*reac); err != nil {
		logf("update reaction status %s: %v", status, err)
	}
}

// notifyReaction delivers a reaction's outcome to the reviewer, preferring the
// chat where the /sat act command was issued and falling back to the configured
// reviewer recipient.
func (s *Service) notifyReaction(reac store.ReviewReaction) {
	if !s.lark.Enabled() {
		return
	}
	req, err := s.store.GetReview(reac.ReviewID)
	if err != nil {
		logf("notifyReaction: load review %s: %v", reac.ReviewID, err)
		return
	}
	text := s.FormatReaction(reac, req)
	switch {
	case reac.ChatID != "":
		err = s.lark.SendText("chat_id", reac.ChatID, text)
	case s.cfg.ReviewerOpenID != "":
		err = s.lark.SendTextToUser(s.cfg.ReviewerOpenID, text)
	case s.cfg.ReviewerChatID != "":
		err = s.lark.SendText("chat_id", s.cfg.ReviewerChatID, text)
	}
	if err != nil {
		logf("notify reaction: %v", err)
	}
	if s.cfg.ReplyInThread && reac.MessageID != "" {
		if err := s.lark.ReplyMessage(reac.MessageID, text); err != nil {
			logf("reply reaction in thread: %v", err)
		}
	}
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "\n...[truncated]"
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "saturnus-review: "+format+"\n", args...)
}
