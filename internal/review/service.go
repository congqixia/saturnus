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

type MessageContext struct {
	Text          string
	ChatID        string
	SenderOpenID  string
	SenderName    string
	MessageID     string
	RootMessageID string
	Topic         string
}

type Service struct {
	store  *store.Store
	lark   *lark.Client
	cfg    Config
	jobs   chan string
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

func New(st *store.Store, lk *lark.Client, cfg Config) *Service {
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
		store:  st,
		lark:   lk,
		cfg:    cfg,
		jobs:   make(chan string, 64),
		ctx:    ctx,
		cancel: cancel,
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

func (s *Service) Start() {
	if !s.cfg.Enabled {
		return
	}
	if _, err := s.store.FailInterruptedReviews(); err != nil {
		logf("fail interrupted reviews: %v", err)
	}
	for i := 0; i < s.cfg.MaxConcurrent; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	for _, req := range s.store.ListReviews("pending") {
		s.enqueue(req.ID)
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
	for id := range s.jobs {
		s.process(id)
	}
}

func (s *Service) enqueue(id string) {
	select {
	case s.jobs <- id:
	default:
		go func() { s.jobs <- id }()
	}
}

func (s *Service) Submit(ctx MessageContext) (store.ReviewRequest, error) {
	if !s.cfg.Enabled {
		return store.ReviewRequest{}, errors.New("PR review is disabled")
	}
	if !s.allowed(ctx.SenderOpenID) {
		return store.ReviewRequest{}, errors.New("sender not allowed to request PR review")
	}
	pr, err := FindPRRequest(ctx.Text)
	if err != nil {
		return store.ReviewRequest{}, err
	}
	if _, ok := s.repoDirFor(pr.Repo); !ok {
		return store.ReviewRequest{}, fmt.Errorf("repo %s is not in the review whitelist (SATURNUS_REVIEW_REPOS)", pr.Repo)
	}
	for _, existing := range s.store.ListReviews("pending") {
		if existing.Repo == pr.Repo && existing.PRNumber == pr.PRNumber {
			return existing, errors.New("review already pending: " + existing.ID)
		}
	}
	thread, err := s.store.GetOrCreateReviewThread(ctx.ChatID, ctx.RootMessageID, ctx.Topic, []string{ctx.SenderOpenID})
	if err != nil {
		return store.ReviewRequest{}, err
	}
	requesterName := ctx.SenderName
	if requesterName == "" && s.lark.Enabled() {
		if name, err := s.lark.GetUserName(ctx.SenderOpenID); err != nil {
			logf("resolve requester name: %v", err)
		} else {
			requesterName = name
		}
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
	if s.cfg.CreateTask {
		taskID, err := s.createTask(req)
		if err != nil {
			logf("create feishu task: %v", err)
		} else if taskID != "" {
			req.TaskID = taskID
			if _, err := s.store.UpdateReview(req); err != nil {
				logf("store task id: %v", err)
			}
		}
	}
	s.enqueue(req.ID)
	return req, nil
}

func (s *Service) List(status string) []store.ReviewRequest {
	return s.store.ListReviews(status)
}

func (s *Service) Get(id string) (store.ReviewRequest, error) {
	return s.store.GetReview(id)
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

func (s *Service) createTask(req store.ReviewRequest) (string, error) {
	if !s.lark.Enabled() {
		return "", nil
	}
	return s.lark.CreateTask(lark.CreateTaskInput{
		Summary:      fmt.Sprintf("Review PR %s#%d", req.Repo, req.PRNumber),
		Description:  fmt.Sprintf("PR review request %s\n%s", req.ID, req.PRURL),
		Due:          time.Now().Add(time.Duration(s.cfg.TaskDueHours) * time.Hour),
		MemberOpenID: s.cfg.ReviewerOpenID,
		SourceTitle:  "Saturnus PR Review",
		SourceURL:    req.PRURL,
	})
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

	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Timeout)
	defer cancel()

	info := ResolvePRInfo(ctx, s.cfg.GHCli, req.Repo, req.PRNumber)
	if info.Title != "" && req.Title == "" {
		req.Title = info.Title
		req.BaseBranch = info.BaseBranch
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

	args, err := RenderCommand(s.cfg.CommandTemplate, ToolArgs{
		Repo:       req.Repo,
		PRNumber:   req.PRNumber,
		PRURL:      req.PRURL,
		Title:      req.Title,
		BaseBranch: req.BaseBranch,
		Worktree:   repoRoot,
		Guide:      guide,
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
	clean := stripANSI(output)
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
	_, _ = fmt.Fprintf(f, "== review %s started %s ==\n", id, time.Now().UTC().Format(time.RFC3339))
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
	}
	if req.Error != "" {
		fmt.Fprintf(&b, "\nerror=%s", req.Error)
	}
	clean := stripANSI(req.ResultText)
	if summary := extractSummary(clean); summary != "" {
		fmt.Fprintf(&b, "\n\nREVIEW SUMMARY:\n%s", truncate(summary, 2000))
	} else if clean != "" {
		fmt.Fprintf(&b, "\n\n%s", truncate(clean, 1500))
	}
	if s.cfg.LogDir != "" {
		fmt.Fprintf(&b, "\n\nlog=%s", filepath.Join(s.cfg.LogDir, req.ID+".log"))
	}
	return b.String()
}

func (s *Service) resolveRequesterName(req *store.ReviewRequest) {
	if req.RequesterName != "" || req.RequesterOpenID == "" || !s.lark.Enabled() {
		return
	}
	name, err := s.lark.GetUserName(req.RequesterOpenID)
	if err != nil {
		logf("resolve requester name for %s: %v", req.ID, err)
		return
	}
	if name == "" {
		return
	}
	req.RequesterName = name
	if _, err := s.store.UpdateReview(*req); err != nil {
		logf("store requester name for %s: %v", req.ID, err)
	}
}

func (s *Service) notifyReviewer(req store.ReviewRequest) {
	s.resolveRequesterName(&req)
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

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "\n...[truncated]"
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "saturnus-review: "+format+"\n", args...)
}
