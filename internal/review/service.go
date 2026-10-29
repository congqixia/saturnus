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

	"saturnus/internal/keylock"
	"saturnus/internal/lark"
	"saturnus/internal/repoconfig"
	"saturnus/internal/reviewlog"
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
	// DisableLocalFallback keeps reviews pending when no remote worker is
	// available, without removing checkout mappings shared with Tasks.
	DisableLocalFallback bool
	Timeout              time.Duration
	CreateTask           bool
	TaskDueHours         int
	// TaskGroup is the name of the Feishu task group (任务分组) tasks are
	// filed under. When set, Saturnus looks it up and creates it if missing.
	TaskGroup string
	// TaskGroupID pins an existing task group by guid, bypassing the name
	// lookup/create.
	TaskGroupID string
	GHCli       string
	LogDir      string
	Guides      map[string]string
	// ResultAPIURL is the callback the review tool is instructed to POST the
	// machine-readable review result to. It may contain the placeholder
	// {review_id}, substituted per review. Empty disables the structured-result
	// prompt block (the tool falls back to the text REVIEW SUMMARY block).
	ResultAPIURL string
	// ResultToken is an optional bearer token required by ResultAPIURL; when
	// set it is echoed into the prompt so the tool can authenticate.
	ResultToken string
	// PostInlineComments, when enabled, makes the server post the review's
	// findings as GitHub inline comments (and a formal PR review mapped from
	// the verdict) once the review succeeds with a structured verdict.
	PostInlineComments bool
	// PostInlineMinSeverity is the minimum finding severity posted as an
	// inline comment: P2 (default) posts everything, P1 posts only P0/P1.
	PostInlineMinSeverity string
	// NotifyRequester, when enabled, DMs the requester a short summary once a
	// review succeeds with a structured verdict: verdict, finding counts and
	// how many inline comments were posted to the PR.
	NotifyRequester bool
}

var ErrAlreadyPending = errors.New("review already in progress")

// ErrReviewUnchanged is returned when re-requesting a review whose PR head
// commit has not changed since the last (successful) review.
var ErrReviewUnchanged = errors.New("review is already up to date")

// ErrArchived is returned when a review was archived (its PR was merged or
// closed) and a re-run is attempted.
var ErrArchived = errors.New("review is archived")

// Finding is one concrete issue identified by the review tool, graded by
// severity (P0 = must fix, P1 = should fix, P2 = nit). It is the wire format
// the tool POSTs and the row stored in review_findings. On a retry/continue
// round the tool references prior findings by their id and reports a new
// Status (open / addressed / commented) instead of re-creating them.
type Finding struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Severity   string `json:"severity"`
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Title      string `json:"title"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion"`
}

// validVerdicts, validSeverities and validFindingStatuses are the accepted
// verdicts, severities and finding statuses.
var (
	validVerdicts        = map[string]bool{"lgtm": true, "request_change": true, "nit_comments": true}
	validSeverities      = map[string]bool{"P0": true, "P1": true, "P2": true}
	validFindingStatuses = map[string]bool{"open": true, "addressed": true, "commented": true}
)

type MessageContext struct {
	Text          string
	ChatID        string
	ChatType      string
	SenderOpenID  string
	SenderName    string
	MessageID     string
	ReplyTo       string
	RootMessageID string
	Topic         string
	// Mentions holds the open_ids of every user @mentioned in the message
	// (the special value "all" marks @everyone). MentionNames maps those ids
	// to their display names as rendered by the client.
	Mentions     []string
	MentionNames map[string]string
	// RequesterOpenID explicitly attributes the review requester (who receives
	// result notifications and task assignment) to a different user than the
	// command sender, e.g. via "@user" in /sat review. Empty means the sender
	// is the requester.
	RequesterOpenID string
}

// EffectiveRequesterOpenID returns the requester for a review: the explicitly
// attributed open_id when set, otherwise the message sender.
func (ctx MessageContext) EffectiveRequesterOpenID() string {
	if ctx.RequesterOpenID != "" {
		return ctx.RequesterOpenID
	}
	return ctx.SenderOpenID
}

type larkClient interface {
	Enabled() bool
	GetUserName(openID string) (string, error)
	SendText(receiveIDType, receiveID, text string) error
	SendTextToUser(openID, text string) error
	CreateTask(input lark.CreateTaskInput) (lark.Task, error)
	GetTask(taskGUID string) (lark.Task, error)
	GetTaskContext(context.Context, string) (lark.Task, error)
	UpdateTaskContext(context.Context, string, lark.UpdateTaskInput) error
	UpdateTask(taskGUID string, input lark.UpdateTaskInput) error
	ReplyMessage(messageID, text string) error
	EnsureTaskList(name string) (lark.TaskList, error)
	AddTaskToList(taskGUID, taskListGUID string) error
}

// job is one unit of work for the worker pool: a review execution or a
// reviewer-triggered reaction on a finished review.
type job struct {
	kind string // "review" or "reaction"
	id   string
}

type Service struct {
	syncLocks keylock.Map
	store     *store.Store
	lark      larkClient
	cfg       Config
	jobs      chan job
	wg        sync.WaitGroup
	ctx       context.Context
	cancel    context.CancelFunc

	// nameFailures tracks recent requester-name resolution failures by open_id
	// so repeated lookups (e.g. web UI polling) do not hammer the contact API
	// or spam the logs. Keyed by open_id, value is the failure time.
	nameFailures   map[string]time.Time
	nameFailuresMu sync.Mutex

	// Task group resolution is cached once: the guid of the configured task
	// group (SATURNUS_REVIEW_TASK_GROUP / _ID), resolved lazily on first use.
	taskListOnce sync.Once
	taskListGUID string
	taskListErr  error

	// onJob, when set (distributed mode), routes newly available jobs to the
	// scheduler instead of the local worker pool. The scheduler decides whether
	// to dispatch to a remote worker or fall back to local execution.
	onJob func(kind, id string)

	// repoProvider, when set (distributed mode), is consulted at submission
	// time to accept repos served by registered remote workers, in addition to
	// the server's own checkouts.
	repoProvider RepoProvider
}

// RepoProvider reports whether a repo can be served by some registered review
// worker. It is implemented by the scheduler in distributed mode.
type RepoProvider interface {
	RepoAvailable(repo string) bool
}

// SetRepoProvider installs the worker-repo registry consulted at submission
// time (distributed mode).
func (s *Service) SetRepoProvider(p RepoProvider) {
	s.repoProvider = p
}

// repoServed reports whether a repo is accepted for review: the server has its
// own checkout, or a registered worker can serve it.
func (s *Service) repoServed(repo string) bool {
	if _, ok := s.repoDirFor(repo); ok {
		return true
	}
	return s.repoProvider != nil && s.repoProvider.RepoAvailable(repo)
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
	if cfg.PostInlineMinSeverity == "" {
		cfg.PostInlineMinSeverity = "P2"
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
	return repoconfig.ParseInline(value)
}

func (s *Service) Enabled() bool {
	return s.cfg.Enabled
}

// Tool returns the configured review tool name (e.g. "opencode").
func (s *Service) Tool() string {
	return s.cfg.Tool
}

// GHCli returns the configured GitHub CLI executable (default "gh").
func (s *Service) GHCli() string {
	return s.cfg.GHCli
}

// ResultToken returns the configured bearer token required to submit a
// structured review result (empty when the endpoint is unauthenticated).
func (s *Service) ResultToken() string {
	return s.cfg.ResultToken
}

// RepoList returns the locally configured repo names (owner/name).
func (s *Service) RepoList() []string {
	repos := make([]string, 0, len(s.cfg.Repos))
	for repo := range s.cfg.Repos {
		repos = append(repos, repo)
	}
	return repos
}

func (s *Service) Start() {
	if !s.cfg.Enabled {
		return
	}
	if s.onJob == nil {
		// Local-only mode: reviews/reactions interrupted by a restart are failed
		// here and re-enqueued below. In distributed mode the scheduler owns
		// worker-aware recovery.
		if _, err := s.store.FailInterruptedReviews(); err != nil {
			logf("fail interrupted reviews: %v", err)
		}
		if _, err := s.store.FailInterruptedReactions(); err != nil {
			logf("fail interrupted reactions: %v", err)
		}
	}
	s.BackfillRequesterNames()
	for i := 0; i < s.cfg.MaxConcurrent; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	if s.onJob == nil {
		for _, req := range s.store.ListReviews("pending", false) {
			s.enqueue("review", req.ID)
		}
		for _, reac := range s.store.ListPendingReactions() {
			s.enqueue("reaction", reac.ID)
		}
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

// SetJobSink installs a scheduler callback that receives every newly available
// job instead of the local worker pool. The scheduler decides remote vs local
// execution. Setting a sink also disables the local-only restart recovery (the
// scheduler performs worker-aware recovery of its own).
func (s *Service) SetJobSink(fn func(kind, id string)) {
	s.onJob = fn
}

// enqueue routes a job to the scheduler when one is installed, otherwise to the
// local worker pool.
func (s *Service) enqueue(kind, id string) {
	if s.onJob != nil {
		s.onJob(kind, id)
		return
	}
	s.enqueueLocal(kind, id)
}

func (s *Service) enqueueLocal(kind, id string) {
	j := job{kind: kind, id: id}
	select {
	case s.jobs <- j:
	default:
		go func() { s.jobs <- j }()
	}
}

// IngestResult stores a machine-readable review result POSTed by the review
// tool while a review is running. It validates the verdict and findings,
// records the verdict/summary on the review and its run atomically, and
// persists the findings. Findings carrying an id reference a finding from a
// previous round and update its status/content in place; findings without an
// id are new. The tool is expected to POST once per run; a second submission
// against an already-recorded result is rejected.
func (s *Service) IngestResult(reviewID, verdict, summary string, findings []Finding) error {
	return s.IngestRunResult(reviewID, "", verdict, summary, findings)
}

// IngestRunResult fences callbacks from reset/retried executions. Empty runID
// remains supported for legacy clients; newly dispatched callbacks carry it.
func (s *Service) IngestRunResult(reviewID, runID, verdict, summary string, findings []Finding) error {
	if !s.cfg.Enabled {
		return errors.New("PR review is disabled")
	}
	if reviewID == "" {
		return errors.New("empty review id")
	}
	unlock, err := s.syncLocks.Lock(context.Background(), reviewID)
	if err != nil {
		return err
	}
	defer unlock()
	req, err := s.store.GetReview(reviewID)
	if err != nil {
		return err
	}
	run, runErr := s.store.LatestRun(reviewID)
	if runErr != nil && !errors.Is(runErr, store.ErrNotFound) {
		return runErr
	}
	if runID != "" && (runErr != nil || run.ID != runID || (run.Status != "running" && run.Status != "succeeded")) {
		return ErrStaleResult
	}
	if runID != "" && run.Verdict != "" {
		return fmt.Errorf("review run %s already has a structured result", runID)
	}
	switch req.Status {
	case "reviewing":
		// The worker is executing the tool; this is the normal submission point.
	case "succeeded":
		// Covers the race where the tool POSTs just as the worker marks the run
		// finished. Only accept while no structured result is recorded yet, so a
		// late duplicate cannot clobber an existing one.
		if req.Verdict != "" {
			return fmt.Errorf("review %s already has a structured result", reviewID)
		}
	default:
		return fmt.Errorf("review %s is not running (status=%s); results can only be submitted while a review run is in progress", reviewID, req.Status)
	}

	verdict = strings.ToLower(strings.TrimSpace(verdict))
	if !validVerdicts[verdict] {
		return fmt.Errorf("invalid verdict %q; must be one of lgtm, request_change, nit_comments", verdict)
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return errors.New("summary is required")
	}
	existing := map[string]bool{}
	for _, f := range s.store.ListFindings(reviewID) {
		existing[f.ID] = true
	}
	for i := range findings {
		f := &findings[i]
		f.ID = strings.TrimSpace(f.ID)
		f.Status = strings.ToLower(strings.TrimSpace(f.Status))
		if f.ID != "" {
			// Status update for a prior finding.
			if !existing[f.ID] {
				return fmt.Errorf("finding #%d references unknown id %q (not part of this review)", i+1, f.ID)
			}
			if f.Status != "" && !validFindingStatuses[f.Status] {
				return fmt.Errorf("finding #%d has invalid status %q; must be open, addressed or commented", i+1, f.Status)
			}
			continue
		}
		// New finding.
		f.Severity = strings.ToUpper(strings.TrimSpace(f.Severity))
		if !validSeverities[f.Severity] {
			return fmt.Errorf("finding #%d has invalid severity %q; must be P0, P1 or P2", i+1, f.Severity)
		}
		if strings.TrimSpace(f.Path) == "" {
			return fmt.Errorf("finding #%d has an empty path", i+1)
		}
		if strings.TrimSpace(f.Message) == "" {
			return fmt.Errorf("finding #%d has an empty message", i+1)
		}
		if f.Status != "" && !validFindingStatuses[f.Status] {
			return fmt.Errorf("finding #%d has invalid status %q; must be open, addressed or commented", i+1, f.Status)
		}
	}
	if runErr == nil {
		runID = run.ID
	}
	storeFindings := make([]store.ReviewFinding, 0, len(findings))
	for _, f := range findings {
		storeFindings = append(storeFindings, store.ReviewFinding{
			ID:         f.ID,
			Status:     f.Status,
			Severity:   f.Severity,
			Path:       f.Path,
			Line:       f.Line,
			Title:      f.Title,
			Message:    f.Message,
			Suggestion: f.Suggestion,
		})
	}
	if err := s.store.SaveReviewResult(reviewID, runID, verdict, summary, storeFindings); err != nil {
		return err
	}
	logf("ingested structured review result for %s: verdict=%s findings=%d", reviewID, verdict, len(findings))
	return nil
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
	if !s.repoServed(pr.Repo) {
		return store.ReviewRequest{}, fmt.Errorf("repo %s is not served by any review worker (SATURNUS_REVIEW_REPOS or SATURNUS_REVIEW_REPOS_FILE)", pr.Repo)
	}
	thread, err := s.store.GetOrCreateReviewThread(ctx.ChatID, ctx.RootMessageID, ctx.Topic, []string{ctx.SenderOpenID})
	if err != nil {
		return store.ReviewRequest{}, err
	}
	requesterOpenID := ctx.EffectiveRequesterOpenID()
	requesterName := s.resolveNameFor(ctx, requesterOpenID)

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
		return s.retryFinished(&existing, ctx, reason)
	}

	req := store.ReviewRequest{
		ThreadID:        thread.ID,
		Status:          "pending",
		PRURL:           pr.PRURL,
		Repo:            pr.Repo,
		PRNumber:        pr.PRNumber,
		RequesterOpenID: requesterOpenID,
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
// always retried; successful reviews are retried when the PR head moved or a
// required structured result is missing. The previous tool session is kept so
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
	if req.Status == "succeeded" && s.cfg.ResultAPIURL != "" && req.Verdict == "" {
		reason = "retry after missing structured review result"
	} else if req.Status == "succeeded" {
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
	return s.retryFinished(&req, ctx, reason)
}

// RetryOnReply handles a reply (with a bot mention) inside a review thread:
// it looks up the latest active review rooted at that thread and re-checks
// whether the PR head moved. When it did (or the review has no recorded
// baseline, e.g. a failed run), the review is re-queued and both the requester
// (in-thread) and the reviewer are notified. When nothing changed it returns
// retried=false so the caller can acknowledge the reply without a re-run.
func (s *Service) RetryOnReply(ctx MessageContext) (store.ReviewRequest, bool, error) {
	if !s.cfg.Enabled {
		return store.ReviewRequest{}, false, errors.New("PR review is disabled")
	}
	req, err := s.store.GetLatestReviewByThread(ctx.ChatID, ctx.RootMessageID)
	if err != nil {
		return store.ReviewRequest{}, false, errors.New("no active review found in this thread")
	}
	switch req.Status {
	case "pending", "reviewing":
		return req, false, nil
	case "archived":
		return req, false, ErrArchived
	}
	changed, head := s.headChanged(&req)
	if !changed {
		return req, false, nil
	}
	reason := "reply in review thread; PR head changed"
	if head != "" {
		reason += " to " + head
	}
	retried, err := s.retryFinished(&req, ctx, reason)
	if err != nil {
		return store.ReviewRequest{}, false, err
	}
	s.notifyReplyRetry(retried, ctx, head)
	return retried, true, nil
}

// notifyReplyRetry tells the requester (in-thread) that the re-review has
// started and pings the reviewer about the trigger.
func (s *Service) notifyReplyRetry(req store.ReviewRequest, ctx MessageContext, head string) {
	if !s.lark.Enabled() {
		return
	}
	var requesterNote string
	if head != "" {
		requesterNote = fmt.Sprintf("重新评审已开始：%s#%d (%s)，PR head 已更新至 %s。完成后会通知你。", req.Repo, req.PRNumber, req.ID, shortHead(head))
	} else {
		requesterNote = fmt.Sprintf("重新评审已开始：%s#%d (%s)。完成后会通知你。", req.Repo, req.PRNumber, req.ID)
	}
	if ctx.MessageID != "" {
		if err := s.lark.ReplyMessage(ctx.MessageID, requesterNote); err != nil {
			logf("reply retry notice: %v", err)
		}
	} else if req.RequesterOpenID != "" {
		if err := s.lark.SendTextToUser(req.RequesterOpenID, requesterNote); err != nil {
			logf("notify requester of re-review: %v", err)
		}
	}

	reviewerNote := fmt.Sprintf("Review %s (%s#%d) 已重新运行：PR head 变更，由 review 线程内的回复触发。requester=%s", req.ID, req.Repo, req.PRNumber, requesterDisplayName(req))
	switch {
	case s.cfg.ReviewerOpenID != "":
		if err := s.lark.SendTextToUser(s.cfg.ReviewerOpenID, reviewerNote); err != nil {
			logf("notify reviewer of re-review: %v", err)
		}
	case s.cfg.ReviewerChatID != "":
		if err := s.lark.SendText("chat_id", s.cfg.ReviewerChatID, reviewerNote); err != nil {
			logf("notify reviewer of re-review: %v", err)
		}
	}
}

// shortHead returns a short commit prefix for messages.
func shortHead(head string) string {
	if len(head) > 8 {
		return head[:8]
	}
	return head
}

// requesterDisplayName returns the requester's stored display name or open_id.
func requesterDisplayName(req store.ReviewRequest) string {
	if req.RequesterName != "" {
		return req.RequesterName
	}
	return req.RequesterOpenID
}

// retryFinished resets a finished review to pending and re-queues it. The
// existing SessionID and RetryReason are kept so process() can resume the same
// tool conversation and explain why the run is happening.
//
// The requester identity is preserved across re-runs: a re-submission or retry
// by anyone else (typically the reviewer continuing the review) must not
// reassign who receives requester replies and task notifications. Only when the
// recorded requester is a placeholder (empty, or the HTTP-API "api" sentinel)
// is the new sender (or an explicitly attributed requester) adopted. The
// chat/message/thread context is still updated so the latest request is
// recorded in the review thread.
func (s *Service) retryFinished(existing *store.ReviewRequest, ctx MessageContext, reason string) (store.ReviewRequest, error) {
	lockCtx := s.ctx
	if lockCtx == nil {
		lockCtx = context.Background()
	}
	unlock, err := s.syncLocks.Lock(lockCtx, existing.ID)
	if err != nil {
		return store.ReviewRequest{}, err
	}
	defer unlock()
	fresh, err := s.store.GetReview(existing.ID)
	if err != nil {
		return store.ReviewRequest{}, err
	}
	if fresh.Status == "pending" || fresh.Status == "reviewing" {
		return fresh, ErrAlreadyPending
	}
	if fresh.Status == "archived" {
		return fresh, ErrArchived
	}
	*existing = fresh

	existing.Status = "pending"
	existing.ResultText, existing.Error = "", ""
	existing.Verdict, existing.Summary = "", ""
	existing.CompletedAt = time.Time{}
	existing.CommentsPostedAt = time.Time{}
	existing.RetryReason = reason
	if isPlaceholderRequester(existing.RequesterOpenID) {
		existing.RequesterOpenID = ctx.EffectiveRequesterOpenID()
		existing.RequesterName = s.resolveNameFor(ctx, existing.RequesterOpenID)
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

// Refresh keeps sender authorization at the manual entry point.
func (s *Service) Refresh(id string, message MessageContext) (store.ReviewRequest, RefreshResult, error) {
	if !s.cfg.Enabled {
		return store.ReviewRequest{}, RefreshResult{}, errors.New("PR review is disabled")
	}
	if !s.allowed(message.SenderOpenID) {
		return store.ReviewRequest{}, RefreshResult{}, errors.New("sender not allowed to refresh a review")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := s.store.GetReview(id)
	if err != nil {
		return req, RefreshResult{}, err
	}
	info := ResolvePRInfo(ctx, s.cfg.GHCli, req.Repo, req.PRNumber)
	return s.syncReview(ctx, id, info)
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

// resolveNameFor resolves a display name for an open_id, preferring the name
// carried in the message (mention name, or the sender name when the id is the
// sender), then falling back to the contact API.
func (s *Service) resolveNameFor(ctx MessageContext, openID string) string {
	if name := ctx.MentionNames[openID]; name != "" {
		return name
	}
	if openID == ctx.SenderOpenID && ctx.SenderName != "" {
		return ctx.SenderName
	}
	if openID != "" && s.lark.Enabled() {
		if n, err := s.lark.GetUserName(openID); err != nil {
			logf("resolve requester name: %v", err)
		} else {
			return n
		}
	}
	return ""
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
	if err := s.store.SetReviewTask(req.ID, req.TaskID, req.TaskURL); err != nil {
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

// SetRequester reassigns the requester of an existing review (who receives
// result notifications, requester replies and task assignment). The name is
// resolved from the contact API when not provided. Archived reviews are
// immutable and rejected.
func (s *Service) SetRequester(id, openID, name string) (store.ReviewRequest, error) {
	if !s.cfg.Enabled {
		return store.ReviewRequest{}, errors.New("PR review is disabled")
	}
	if openID == "" {
		return store.ReviewRequest{}, errors.New("empty requester open_id")
	}
	req, err := s.store.GetReview(id)
	if err != nil {
		return store.ReviewRequest{}, err
	}
	if req.Status == "archived" {
		return store.ReviewRequest{}, ErrArchived
	}
	req.RequesterOpenID = openID
	if name == "" && s.lark.Enabled() {
		if n, err := s.lark.GetUserName(openID); err != nil {
			logf("resolve requester name: %v", err)
		} else {
			name = n
		}
	}
	req.RequesterName = name
	if err := s.store.SetReviewRequester(req.ID, req.RequesterOpenID, req.RequesterName); err != nil {
		return store.ReviewRequest{}, err
	}
	return req, nil
}

// ListRuns returns every recorded run of a review in execution order.
func (s *Service) ListRuns(reviewID string) []store.ReviewRun {
	return s.store.ListRuns(reviewID)
}

// ReviewOutdated reports whether a review's last run reviewed an older PR head
// than the PR's current head as recorded by the last refresh (pr_head_commit).
// A review with no recorded head on either side is never flagged.
func ReviewOutdated(req store.ReviewRequest) bool {
	return req.PrHeadCommit != "" && req.HeadCommit != "" && req.PrHeadCommit != req.HeadCommit
}

// WorkerDescription includes the registered host when it is still available.
func (s *Service) WorkerDescription(req store.ReviewRequest) string {
	label := WorkerLabel(req.Status, req.WorkerID)
	if req.WorkerID != "" && s.store != nil {
		if worker, err := s.store.GetWorker(req.WorkerID); err == nil && worker.Host != "" {
			label += " (host: " + worker.Host + ")"
		}
	}
	return label
}

func VerdictAbbreviation(verdict string) string {
	switch verdict {
	case "lgtm":
		return "LGTM"
	case "nit_comments":
		return "NIT"
	case "request_change":
		return "RC"
	default:
		return ""
	}
}

// LastVerdict preserves the last conclusion in lists while retry/reset has
// cleared the current result or a later execution has not produced one.
func (s *Service) LastVerdict(req store.ReviewRequest) string {
	if req.Verdict != "" || s.store == nil {
		return req.Verdict
	}
	verdict, err := s.store.LatestReviewVerdict(req.ID)
	if err != nil {
		logf("load last verdict review_id=%s: %v", req.ID, err)
	}
	return verdict
}

// ReviewStatusLabel is shared by the list card and its text fallback.
func ReviewStatusLabel(req store.ReviewRequest, verdict string) string {
	status := req.Status
	if short := VerdictAbbreviation(verdict); short != "" {
		status += " / " + short
	}
	if ReviewOutdated(req) {
		status += " / OUTDATED"
	}
	return status
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
		status := ReviewStatusLabel(req, s.LastVerdict(req))
		fmt.Fprintf(&b, "%s %s %s#%d worker=%s %s\n", req.ID, status, req.Repo, req.PRNumber, WorkerLabel(req.Status, req.WorkerID), req.CreatedAt.Format(time.RFC3339))
	}
	return strings.TrimSpace(b.String())
}

func (s *Service) FormatOne(req store.ReviewRequest) string {
	var b strings.Builder
	status := req.Status
	if ReviewOutdated(req) {
		status += " OUTDATED"
	}
	fmt.Fprintf(&b, "%s status=%s\n%s\nrepo=%s#%d", req.ID, status, req.PRURL, req.Repo, req.PRNumber)
	fmt.Fprintf(&b, "\nworker=%s", s.WorkerDescription(req))
	if contextID, err := s.ContextID(req.ID); err == nil && contextID != "" {
		fmt.Fprintf(&b, "\ncontext=%s", contextID)
	}
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
	if req.PrHeadCommit != "" && req.PrHeadCommit != req.HeadCommit {
		fmt.Fprintf(&b, "\npr_head=%s (outdated)", req.PrHeadCommit)
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
	if req.Verdict != "" {
		fmt.Fprintf(&b, "\nverdict=%s", req.Verdict)
	}
	if !req.CommentsPostedAt.IsZero() {
		fmt.Fprintf(&b, "\ncomments_posted_at=%s", req.CommentsPostedAt.Format(time.RFC3339))
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
	lockCtx := s.ctx
	if lockCtx == nil {
		lockCtx = context.Background()
	}
	unlock, err := s.syncLocks.Lock(lockCtx, reviewID)
	if err != nil {
		return store.ReviewReaction{}, err
	}
	defer unlock()
	req, err := s.store.GetReview(reviewID)
	if err != nil {
		return store.ReviewReaction{}, err
	}
	if req.Status == "pending" || req.Status == "reviewing" {
		return store.ReviewReaction{}, errors.New("review is still in progress; wait for it to finish first")
	}
	// Resolve the tool session to resume now so a missing one fails fast
	// instead of enqueuing a job that cannot run. For reviews that ran on a
	// remote worker the session lives on that host, so the local checkout is
	// not required here; the scheduler routes the reaction back to the owning
	// worker, which resolves the session itself.
	remote := req.WorkerID != ""
	sessionID := req.SessionID
	if !remote {
		repoRoot, ok := s.repoDirFor(req.Repo)
		if !ok {
			return store.ReviewReaction{}, fmt.Errorf("repo %s is not in the review whitelist (SATURNUS_REVIEW_REPOS or SATURNUS_REVIEW_REPOS_FILE)", req.Repo)
		}
		if sessionID == "" {
			sessionID = s.captureSessionID(repoRoot, req.ID)
		}
		if sessionID == "" {
			return store.ReviewReaction{}, errors.New("review has no tool session to continue; run the review first")
		}
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
		return "No repos whitelisted. Set SATURNUS_REVIEW_REPOS_FILE to a JSON mapping file, or SATURNUS_REVIEW_REPOS=owner/repo=/path/to/checkout,..."
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
	task, err := s.lark.CreateTask(lark.CreateTaskInput{
		Summary:       fmt.Sprintf("Review PR %s#%d", req.Repo, req.PRNumber),
		Description:   fmt.Sprintf("PR review request %s\n%s\nrequester=%s", req.ID, req.PRURL, requester),
		Due:           time.Now().Add(time.Duration(s.cfg.TaskDueHours) * time.Hour),
		MemberOpenIDs: members,
		SourceTitle:   "Saturnus PR Review",
		SourceURL:     req.PRURL,
	})
	if err != nil || task.GUID == "" {
		return task, err
	}
	// File the task under the configured task group, if any.
	guid, gerr := s.taskListGuid()
	if gerr != nil {
		logf("resolve task group: %v", gerr)
	} else if guid != "" {
		if aerr := s.lark.AddTaskToList(task.GUID, guid); aerr != nil {
			logf("add task %s to tasklist %s: %v", task.GUID, guid, aerr)
		}
	}
	return task, nil
}

// taskListGuid returns the guid of the configured task group, resolving it
// once (lookup by name and create when missing, or the pinned guid).
func (s *Service) taskListGuid() (string, error) {
	s.taskListOnce.Do(func() {
		switch {
		case s.cfg.TaskGroupID != "":
			s.taskListGUID = s.cfg.TaskGroupID
		case s.cfg.TaskGroup != "" && s.lark.Enabled():
			list, err := s.lark.EnsureTaskList(s.cfg.TaskGroup)
			if err != nil {
				s.taskListErr = err
				return
			}
			s.taskListGUID = list.GUID
		}
	})
	return s.taskListGUID, s.taskListErr
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

func (s *Service) startLocal(id string) (store.ReviewRequest, store.ReviewRun, error) {
	unlock, err := s.syncLocks.Lock(context.Background(), id)
	if err != nil {
		return store.ReviewRequest{}, store.ReviewRun{}, err
	}
	defer unlock()
	req, err := s.store.AssignReview(id, "")
	if err != nil {
		return store.ReviewRequest{}, store.ReviewRun{}, err
	}
	run, err := s.store.CreateReviewRun(store.ReviewRun{ReviewID: req.ID, Status: "running", RetryReason: req.RetryReason, Tool: s.cfg.Tool})
	if err != nil {
		_ = s.store.UpdateReviewOutcome(req.ID, "failed", "", "create review run: "+err.Error(), time.Now().UTC())
	}
	return req, run, err
}

func (s *Service) process(id string) {
	req, run, err := s.startLocal(id)
	if err != nil {
		logf("start local review %s: %v", id, err)
		return
	}
	reviewlog.Event{Name: "started", Component: "server", Kind: "review", ReviewID: req.ID, RunID: run.ID, WorkerID: "local", Repo: req.Repo, PRNumber: req.PRNumber, Status: "reviewing"}.Log()

	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Timeout)
	defer cancel()

	contextID, err := s.ContextID(req.ID)
	if err != nil {
		s.finish(&req, "failed", "", "read review context: "+err.Error())
		s.notifyReviewer(req)
		return
	}
	info := ResolvePRInfo(ctx, s.cfg.GHCli, req.Repo, req.PRNumber)
	if info.Title != "" && req.Title == "" {
		req.Title = info.Title
		req.BaseBranch = info.BaseBranch
	}
	// Record the PR head commit this run reviews. It is the baseline used to
	// decide whether a later retry/re-submission needs a fresh run. The PR's
	// current head as known here is stored alongside, so the review is not
	// reported OUTDATED right after a run.
	if info.HeadCommit != "" {
		req.HeadCommit = info.HeadCommit
		req.PrHeadCommit = info.HeadCommit
	}
	if req.Title != "" || info.HeadCommit != "" {
		if _, err := s.store.UpdateReview(req); err != nil {
			logf("store pr info: %v", err)
		}
	}

	repoRoot, ok := s.repoDirFor(req.Repo)
	if !ok {
		s.finish(&req, "failed", "", fmt.Sprintf("repo %s not in review whitelist (SATURNUS_REVIEW_REPOS or SATURNUS_REVIEW_REPOS_FILE)", req.Repo))
		s.notifyReviewer(req)
		return
	}
	if !IsGitDir(repoRoot) {
		s.finish(&req, "failed", "", fmt.Sprintf("local checkout not found at %q (check SATURNUS_REVIEW_REPOS or SATURNUS_REVIEW_REPOS_FILE)", repoRoot))
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

	resultAPIURL := s.resultCallbackURL(req.ID, run.ID)
	args, err := RenderCommand(s.cfg.CommandTemplate, ToolArgs{
		Repo:          req.Repo,
		PRNumber:      req.PRNumber,
		PRURL:         req.PRURL,
		Title:         req.Title,
		BaseBranch:    req.BaseBranch,
		Worktree:      repoRoot,
		Guide:         guide,
		ReviewID:      req.ID,
		HeadCommit:    req.HeadCommit,
		SessionID:     req.SessionID,
		SessionTitle:  SessionTitle(req.ID, contextID),
		RetryNote:     retryNote,
		ResultAPIURL:  resultAPIURL,
		ResultToken:   s.cfg.ResultToken,
		PriorFindings: formatPriorFindings(s.store.ListFindings(req.ID)),
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
			if err := s.store.UpdateReviewProgress(req.ID, snapshot); err != nil {
				logf("flush review progress: %v", err)
			}
		}
	}

	reviewlog.Event{Name: "tool_started", Component: "server", Kind: "review", ReviewID: req.ID, RunID: run.ID, WorkerID: "local", Repo: req.Repo, PRNumber: req.PRNumber, Status: "reviewing", Tool: s.cfg.Tool, LogPath: logFile.Name()}.Log()
	output, runErr := Run(ctx, s.cfg.Tool, args, repoRoot, sink)
	if sessionID := s.captureSessionID(repoRoot, req.ID); sessionID != "" {
		req.SessionID = sessionID
		if err := s.store.SetReviewSession(req.ID, sessionID); err != nil {
			logf("store review session review_id=%s: %v", req.ID, err)
		}
	}
	clean := stripANSI(output)
	req.RetryReason = ""
	// Merge any structured result the tool POSTed to the server while this run
	// was executing (the POST handler writes verdict/summary onto the review
	// row, so the run record below can preserve them alongside the text).
	if fresh, err := s.store.GetReview(req.ID); err == nil {
		req.Verdict = fresh.Verdict
		req.Summary = fresh.Summary
	}
	if runErr != nil {
		s.finish(&req, "failed", clean, runErr.Error())
	} else {
		s.finish(&req, "succeeded", clean, "")
	}
	// When enabled and the run produced a structured verdict, publish the
	// findings as GitHub inline comments (plus the verdict review). The marker
	// is set only on full success so a partial failure can be re-triggered.
	if req.Status == "succeeded" && s.cfg.PostInlineComments && req.Verdict != "" && req.CommentsPostedAt.IsZero() {
		if posted, perr := s.PostPRInlineComments(req); perr != nil {
			logf("post inline comments for %s: %v", req.ID, perr)
		} else {
			if posted > 0 {
				logf("posted %d inline comment(s) for %s", posted, req.ID)
			}
			if err := s.store.SetCommentsPosted(req.ID, time.Now().UTC()); err != nil {
				logf("mark comments posted for %s: %v", req.ID, err)
			}
		}
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
	unlock, lockErr := s.syncLocks.Lock(context.Background(), req.ID)
	if lockErr != nil {
		logf("lock review completion %s: %v", req.ID, lockErr)
		return
	}
	defer unlock()
	status, errText = s.completionResult(req, status, result, errText)
	req.Status = status
	req.ResultText = result
	req.Error = errText
	req.CompletedAt = time.Now().UTC()
	runID := ""
	if run, err := s.store.CurrentRun(req.ID); err == nil {
		runID = run.ID
	}
	// Publish the review and run outcome in one transaction, preserving the
	// structured result previously committed by the tool's callback.
	applied, finishErr := s.store.CompleteReviewRun(*req, runID)
	if finishErr == nil && !applied {
		finishErr = errors.New("review completion no longer matches the active owner or run")
	}
	if finishErr != nil {
		logf("update status %s: %v", status, finishErr)
	}
	event := reviewlog.Event{Name: "completed", Component: "server", Kind: "review", ReviewID: req.ID, RunID: runID, WorkerID: "local", Repo: req.Repo, PRNumber: req.PRNumber, Status: status, Error: errText}
	if finishErr != nil {
		event.Name, event.Error = "completion_failed", finishErr.Error()
	}
	event.Log()
}

func (s *Service) FormatResult(req store.ReviewRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PR review %s status=%s\n%s\nrepo=%s#%d", req.ID, req.Status, req.PRURL, req.Repo, req.PRNumber)
	fmt.Fprintf(&b, "\nworker=%s", s.WorkerDescription(req))
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
	if req.Verdict != "" {
		// A machine-readable result was submitted: it is the authoritative
		// outcome, so render it instead of the raw text summary below.
		fmt.Fprintf(&b, "\n\nVerdict: %s", verdictLabel(req.Verdict))
		if req.Summary != "" {
			fmt.Fprintf(&b, "\nSummary: %s", truncate(req.Summary, 1000))
		}
		if s.store != nil {
			if rendered := FormatFindings(s.store.ListFindings(req.ID)); rendered != "" {
				fmt.Fprintf(&b, "\n\n%s", rendered)
			}
		}
		if s.cfg.LogDir != "" {
			fmt.Fprintf(&b, "\n\nlog=%s", filepath.Join(s.cfg.LogDir, req.ID+".log"))
		}
		return b.String()
	}
	if rendered := FormatFindings(s.ListFindings(req.ID)); rendered != "" {
		fmt.Fprintf(&b, "\n\nPrevious findings (current run has no verdict):\n%s", rendered)
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

// verdictLabel maps a stored verdict to a human-friendly display label.
func verdictLabel(v string) string {
	switch v {
	case "lgtm":
		return "LGTM"
	case "request_change":
		return "Request change"
	case "nit_comments":
		return "Nit comments"
	default:
		return v
	}
}

// ListFindings returns every structured finding of a review.
func (s *Service) ListFindings(reviewID string) []store.ReviewFinding {
	if s.store == nil {
		return nil
	}
	return s.store.ListFindings(reviewID)
}

// severityRank orders severities so min-severity filters work: P0 > P1 > P2.
var severityRank = map[string]int{"P0": 3, "P1": 2, "P2": 1}

func severityAtLeast(sev, min string) bool {
	if min == "" {
		return true
	}
	return severityRank[sev] >= severityRank[min]
}

// verdictEvent maps a stored verdict to the GitHub review event it produces:
// lgtm approves, request_change requests changes, nit_comments comments.
func verdictEvent(v string) string {
	switch v {
	case "lgtm":
		return "APPROVE"
	case "request_change":
		return "REQUEST_CHANGES"
	default:
		return "COMMENT"
	}
}

// PostPRInlineComments publishes the review's structured findings on the
// GitHub PR and records each inline comment's id on the finding:
//   - new findings (no recorded comment id) with a concrete path+line and
//     severity at or above PostInlineMinSeverity are posted as inline comments;
//   - findings the tool marked "addressed" have their existing comment thread
//     resolved on GitHub (best-effort) and become status "resolved";
//   - a formal review with the event mapped from the verdict carries the summary.
//
// Posting is best-effort and per-finding: a finding whose line is not part of
// the diff is skipped with a logged error instead of failing the review. The
// method reports how many inline comments were posted. Whether to post at all
// is decided by the callers: the worker's auto path gates on PostInlineComments,
// while the explicit /sat post trigger always posts.
func (s *Service) PostPRInlineComments(req store.ReviewRequest) (int, error) {
	if req.Verdict == "" {
		return 0, nil
	}
	if s.cfg.GHCli == "" {
		return 0, errors.New("gh cli is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	posted := 0
	var errs []string
	for _, f := range s.store.ListFindings(req.ID) {
		if f.GithubCommentID == "" {
			// Not yet posted: post an inline comment when the finding is
			// concrete enough and severe enough.
			if !severityAtLeast(f.Severity, s.cfg.PostInlineMinSeverity) {
				continue
			}
			if strings.TrimSpace(f.Path) == "" || f.Line <= 0 {
				continue
			}
			body := f.Message
			if body == "" {
				body = f.Title
			}
			if f.Suggestion != "" {
				body += "\n\n**suggestion:** " + f.Suggestion
			}
			body += fmt.Sprintf("\n\n_(saturnus review %s, %s)_", req.ID, f.Severity)
			commentID, err := PostInlineComment(ctx, s.cfg.GHCli, req.Repo, req.PRNumber, req.HeadCommit, InlineComment{
				Path: f.Path, Line: f.Line, Body: body,
			})
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			if err := s.store.SetFindingCommentID(f.ID, commentID); err != nil {
				errs = append(errs, err.Error())
			} else {
				posted++
			}
			continue
		}
		// Already posted: resolve the thread when the tool reported the finding
		// as addressed and it is not resolved yet.
		if f.Status == "addressed" && f.Status != "resolved" {
			if err := ResolveInlineComment(ctx, s.cfg.GHCli, req.Repo, req.PRNumber, f.GithubCommentID); err != nil {
				errs = append(errs, err.Error())
			} else if err := s.store.UpdateFindingStatus(f.ID, "resolved"); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}

	// The formal review carries the verdict so the PR's review state matches
	// the structured opinion (approve / request changes / comment).
	body := strings.TrimSpace(req.Summary)
	if body == "" {
		body = "Automated review by Saturnus."
	}
	body += fmt.Sprintf("\n\n_(saturnus review %s, verdict: %s)_", req.ID, verdictLabel(req.Verdict))
	if err := PostPRReview(ctx, s.cfg.GHCli, req.Repo, req.PRNumber, verdictEvent(req.Verdict), body); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return posted, fmt.Errorf("post inline comments: %s", strings.Join(errs, "; "))
	}
	return posted, nil
}

// formatPriorFindings renders the previous rounds' findings for the retry
// prompt so the tool can reference them by id and update their status.
func formatPriorFindings(findings []store.ReviewFinding) string {
	if len(findings) == 0 {
		return ""
	}
	var b strings.Builder
	for _, f := range findings {
		loc := strings.TrimSpace(f.Path)
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", loc, f.Line)
		}
		if loc == "" {
			loc = "-"
		}
		status := f.Status
		if status == "" {
			status = "open"
		}
		title := strings.TrimSpace(f.Title)
		if title == "" {
			title = strings.TrimSpace(f.Message)
		}
		fmt.Fprintf(&b, "- %s [%s] %s (%s)\n", f.ID, f.Severity, truncate(title, 200), loc)
		if msg := strings.TrimSpace(f.Message); msg != "" && msg != title {
			fmt.Fprintf(&b, "  %s\n", truncate(msg, 400))
		}
		fmt.Fprintf(&b, "  status: %s\n", status)
	}
	return strings.TrimSpace(b.String())
}

// PostComments manually publishes a finished review's findings as GitHub
// inline comments (plus the verdict review). It is the path for reviews whose
// findings were never posted — the feature was off when they ran, or posting
// failed then. Posting is skipped when the findings were already published
// (CommentsPostedAt set) so a repeat trigger does not duplicate comments; a
// review without a structured verdict cannot be posted.
func (s *Service) PostComments(id string) (int, error) {
	if !s.cfg.Enabled {
		return 0, errors.New("PR review is disabled")
	}
	req, err := s.store.GetReview(id)
	if err != nil {
		return 0, err
	}
	if req.Status == "archived" {
		return 0, ErrArchived
	}
	if req.Verdict == "" {
		return 0, errors.New("review has no structured verdict; run the review first")
	}
	if !req.CommentsPostedAt.IsZero() {
		return 0, errors.New("review findings were already posted to the PR")
	}
	posted, err := s.PostPRInlineComments(req)
	if err != nil {
		return posted, err
	}
	// Mark published only when the whole attempt succeeded, so a partial
	// failure can be re-triggered to fill the gaps.
	if err := s.store.SetCommentsPosted(req.ID, time.Now().UTC()); err != nil {
		return posted, err
	}
	return posted, nil
}

// FormatFindings renders structured review findings as a bulleted list, one
// bullet per finding with its severity tag and file:line location, so the
// machine-readable result is readable in chat and the web UI alike.
func FormatFindings(findings []store.ReviewFinding) string {
	if len(findings) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Findings:\n")
	for _, f := range findings {
		loc := strings.TrimSpace(f.Path)
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", loc, f.Line)
		}
		if loc == "" {
			loc = "-"
		}
		title := strings.TrimSpace(f.Title)
		if title == "" {
			title = strings.TrimSpace(f.Message)
		}
		fmt.Fprintf(&b, "- [%s] %s (%s)", f.Severity, truncate(title, 300), loc)
		if f.Status != "" && f.Status != "open" {
			fmt.Fprintf(&b, " [%s]", f.Status)
		}
		b.WriteString("\n")
		if msg := strings.TrimSpace(f.Message); msg != "" && msg != title {
			fmt.Fprintf(&b, "  %s\n", truncate(msg, 600))
		}
		if sug := strings.TrimSpace(f.Suggestion); sug != "" {
			fmt.Fprintf(&b, "  Fix: %s\n", truncate(sug, 400))
		}
	}
	return strings.TrimSpace(b.String())
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
	if err := s.store.SetReviewRequesterName(req.ID, req.RequesterOpenID, req.RequesterName); err != nil {
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
	s.notifyRequesterResult(req)
}

// notifyRequesterResult DMs the requester a short summary once a review
// succeeds with a structured verdict: the verdict, the finding count by
// severity, and how many findings were already posted as inline comments on
// the PR (per SATURNUS_REVIEW_POST_INLINE_COMMENTS). Deliberately minimal so
// the full detail stays with the reviewer.
func (s *Service) notifyRequesterResult(req store.ReviewRequest) {
	if !s.cfg.NotifyRequester || req.RequesterOpenID == "" || req.Status != "succeeded" || req.Verdict == "" || !s.lark.Enabled() {
		return
	}
	counts := map[string]int{}
	total, posted := 0, 0
	for _, f := range s.store.ListFindings(req.ID) {
		counts[f.Severity]++
		total++
		if f.GithubCommentID != "" {
			posted++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "PR 评审完成：%s#%d\n%s\n", req.Repo, req.PRNumber, req.PRURL)
	fmt.Fprintf(&b, "Verdict: %s\n", verdictLabel(req.Verdict))
	fmt.Fprintf(&b, "Findings: %d 项 (P0: %d, P1: %d, P2: %d)\n", total, counts["P0"], counts["P1"], counts["P2"])
	if posted > 0 {
		fmt.Fprintf(&b, "已发布 %d 条 inline comment 到 PR\n", posted)
	}
	fmt.Fprintf(&b, "review: %s", req.ID)
	if err := s.lark.SendTextToUser(req.RequesterOpenID, b.String()); err != nil {
		logf("notify requester of result: %v", err)
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
	reac.WorkerID = ""
	if _, err := s.store.UpdateReaction(reac); err != nil {
		logf("mark reaction running: %v", err)
		return
	}
	reviewlog.Event{Name: "started", Component: "server", Kind: "reaction", ReviewID: req.ID, ReactionID: reac.ID, WorkerID: "local", Repo: req.Repo, PRNumber: req.PRNumber, Status: "running"}.Log()

	repoRoot, ok := s.repoDirFor(req.Repo)
	if !ok {
		s.finishReaction(&reac, "failed", "", fmt.Sprintf("repo %s not in review whitelist (SATURNUS_REVIEW_REPOS or SATURNUS_REVIEW_REPOS_FILE)", req.Repo))
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
	contextID, err := s.ContextID(req.ID)
	if err != nil {
		s.finishReaction(&reac, "failed", "", "read review context: "+err.Error())
		s.notifyReaction(reac)
		return
	}
	args := []string{"run", "--auto", "--session", sessionID, "--title", SessionTitle(req.ID, contextID), reac.Instruction}

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

	reviewlog.Event{Name: "tool_started", Component: "server", Kind: "reaction", ReviewID: req.ID, ReactionID: reac.ID, WorkerID: "local", Repo: req.Repo, PRNumber: req.PRNumber, Status: "running", Tool: s.cfg.Tool, LogPath: logFile.Name()}.Log()
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
	event := reviewlog.Event{Name: "completed", Component: "server", Kind: "reaction", ReviewID: reac.ReviewID, ReactionID: reac.ID, WorkerID: "local", Status: status, Error: errText}
	if _, err := s.store.UpdateReaction(*reac); err != nil {
		logf("update reaction status %s: %v", status, err)
		event.Name, event.Error = "completion_failed", err.Error()
	}
	event.Log()
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
