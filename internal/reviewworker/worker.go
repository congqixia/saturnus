// Package reviewworker runs review jobs dispatched by a central saturnus server
// on a remote host. Each worker keeps one WebSocket control connection, reports
// liveness/progress, and executes the review tool against its own local repo
// checkouts.
package reviewworker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"saturnus/internal/ghinfo"
	"saturnus/internal/review"
	"saturnus/internal/reviewlog"
	"saturnus/internal/worker"
	"saturnus/internal/workerproto"
)

// Config is the worker daemon's static configuration.
type Config struct {
	ServerURL     string
	WorkerID      string
	Token         string
	Tool          string
	Repos         map[string]string // owner/repo -> local checkout path
	GH            bool
	GitHub        *ghinfo.Info
	MaxConcurrent int
	LogDir        string
}

// Worker executes jobs dispatched by the server.
type Worker struct {
	cfg      Config
	client   *worker.Client
	sem      chan struct{}
	mu       sync.Mutex
	inFlight map[string]context.CancelFunc
	github   *ghinfo.Info
}

// New builds a Worker from config, applying defaults.
func New(cfg Config) *Worker {
	if cfg.Tool == "" {
		cfg.Tool = "opencode"
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.LogDir == "" {
		cfg.LogDir = os.TempDir() + "/saturnus-review-logs"
	}
	return &Worker{
		cfg:      cfg,
		sem:      make(chan struct{}, cfg.MaxConcurrent),
		inFlight: map[string]context.CancelFunc{},
		github:   cfg.GitHub,
	}
}

// Run connects to the server and serves jobs until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w.client = worker.NewClient(worker.ClientConfig{
		ServerURL: w.cfg.ServerURL,
		WorkerID:  w.cfg.WorkerID,
		Token:     w.cfg.Token,
		InFlight:  w.inFlightList,
		Heartbeat: w.heartbeat,
		OnCommand: w.onCommand,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.refreshGitHub(ctx)
	}()
	err := w.client.Run(ctx)
	cancel()
	<-done
	return err
}

func (w *Worker) refreshGitHub(ctx context.Context) {
	if !w.cfg.GH {
		return
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			info, err := ghinfo.Probe(ctx, "gh")
			if err != nil {
				continue // retain the last observed expiry if gh stops working
			}
			w.updateGitHub(info)
		}
	}
}

func (w *Worker) updateGitHub(info *ghinfo.Info) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if info.ExpiryStatus == "check_failed" && w.github != nil {
		// A temporary probe failure must not erase an already known expiry.
		info.TokenExpiresAt = w.github.TokenExpiresAt
		info.Refreshable = w.github.Refreshable
		info.RefreshTokenExpiresAt = w.github.RefreshTokenExpiresAt
		info.ExpirySource = w.github.ExpirySource
	}
	w.github = info
}

// Close disconnects the control channel.
func (w *Worker) Close() {
	if w.client != nil {
		w.client.Close()
	}
}

func (w *Worker) inFlightList() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.inFlight))
	for id := range w.inFlight {
		out = append(out, id)
	}
	return out
}

func (w *Worker) heartbeat() workerproto.HeartbeatPayload {
	w.mu.Lock()
	defer w.mu.Unlock()
	return workerproto.HeartbeatPayload{
		Load:     len(w.inFlight),
		InFlight: len(w.inFlight),
		ReposOK:  len(w.cfg.Repos),
		GitHub:   w.github,
	}
}

func (w *Worker) onCommand(ctx context.Context, frame workerproto.Frame) error {
	switch frame.Type {
	case workerproto.CmdClaim:
		var p workerproto.JobPayload
		if err := frame.Decode(&p); err != nil {
			return err
		}
		w.dispatch(ctx, p)
	case workerproto.CmdCancel:
		var p workerproto.CancelPayload
		_ = frame.Decode(&p)
		w.cancel(frame.ReviewID)
	}
	return nil
}

// dispatch starts a job, respecting the concurrency limit. When the worker is
// at capacity it replies with a "busy" error so the server can requeue.
func (w *Worker) dispatch(ctx context.Context, p workerproto.JobPayload) {
	w.jobEvent("received", "pending", p).Log()
	select {
	case w.sem <- struct{}{}:
	default:
		event := w.jobEvent("rejected", "busy", p)
		event.Error = "worker at capacity"
		event.Log()
		if err := w.send(workerproto.NewFrame(workerproto.EvError, p.ReviewID, workerproto.ErrorPayload{
			Code:    "busy",
			Message: "worker at capacity",
		})); err != nil {
			event.Name, event.Error = "rejection_report_failed", err.Error()
			event.Log()
		}
		return
	}
	jobCtx, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	w.inFlight[p.ReviewID] = cancel
	w.mu.Unlock()
	go func() {
		defer cancel()
		defer func() {
			<-w.sem
			w.mu.Lock()
			delete(w.inFlight, p.ReviewID)
			w.mu.Unlock()
		}()
		w.jobEvent("started", "running", p).Log()
		if err := w.send(workerproto.NewFrame(workerproto.EvStarted, p.ReviewID, workerproto.StartedPayload{RunID: p.RunID, ReactionID: p.ReactionID})); err != nil {
			event := w.jobEvent("start_report_failed", "running", p)
			event.Error = err.Error()
			event.Log()
		}
		w.runJob(jobCtx, p)
	}()
}

func (w *Worker) cancel(reviewID string) {
	w.mu.Lock()
	cancel, ok := w.inFlight[reviewID]
	w.mu.Unlock()
	if ok {
		reviewlog.Event{Name: "cancel_requested", Component: "worker", ReviewID: reviewID, WorkerID: w.cfg.WorkerID, Status: "running"}.Log()
		cancel()
		_ = w.send(workerproto.NewFrame(workerproto.EvCancelAck, reviewID, nil))
	}
}

func (w *Worker) runJob(ctx context.Context, p workerproto.JobPayload) {
	if p.Kind == workerproto.JobReaction {
		w.runReaction(ctx, p)
		return
	}
	w.runReview(ctx, p)
}

func (w *Worker) jobEvent(name, status string, p workerproto.JobPayload) reviewlog.Event {
	kind := p.Kind
	if kind == "" {
		kind = workerproto.JobReview
	}
	return reviewlog.Event{Name: name, Component: "worker", Kind: kind, ReviewID: p.ReviewID, RunID: p.RunID, ReactionID: p.ReactionID, WorkerID: w.cfg.WorkerID, Repo: p.Repo, PRNumber: p.PRNumber, Status: status}
}

func (w *Worker) complete(p workerproto.JobPayload, ok bool, headCommit, sessionID, result, errText string) {
	status := "succeeded"
	if !ok {
		status = "failed"
	}
	event := w.jobEvent("completed", status, p)
	event.Error = errText
	event.Log()
	err := w.send(workerproto.NewFrame(workerproto.EvComplete, p.ReviewID, workerproto.CompletePayload{
		RunID:      p.RunID,
		OK:         ok,
		HeadCommit: headCommit,
		SessionID:  sessionID,
		ReactionID: p.ReactionID,
		ResultText: result,
		Error:      errText,
	}))
	if err != nil {
		event.Name, event.Error = "completion_report_failed", err.Error()
	} else {
		event.Name, event.Error = "completion_reported", ""
	}
	event.Log()
}

func (w *Worker) send(frame workerproto.Frame) error {
	if w.client == nil {
		return nil
	}
	return w.client.Send(frame)
}

func (w *Worker) runReview(ctx context.Context, p workerproto.JobPayload) {
	dir, ok := w.cfg.Repos[p.Repo]
	if !ok {
		w.complete(p, false, "", "", "", fmt.Sprintf("repo %s is not checked out on this worker (SATURNUS_REVIEW_REPOS or SATURNUS_REVIEW_REPOS_FILE)", p.Repo))
		return
	}
	if !review.IsGitDir(dir) {
		w.complete(p, false, "", "", "", fmt.Sprintf("local checkout not found at %q", dir))
		return
	}

	head, title, base := p.HeadCommit, p.Title, p.BaseBranch
	if w.cfg.GH {
		infoCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		info := review.ResolvePRInfo(infoCtx, "gh", p.Repo, p.PRNumber)
		cancel()
		if info.HeadCommit != "" {
			head = info.HeadCommit
		}
		if title == "" && info.Title != "" {
			title = info.Title
		}
		if base == "" && info.BaseBranch != "" {
			base = info.BaseBranch
		}
	}

	guide := p.Guide
	if guide == "" {
		if inRepo := discoverInRepoGuide(dir); inRepo != "" {
			guide = inRepo
		}
	}

	retryNote := ""
	if p.SessionID != "" {
		baseNote := fmt.Sprintf("Continue the previous review conversation for GitHub PR %s#%d (%s) and produce the final structured review.", p.Repo, p.PRNumber, p.PRURL)
		if p.RetryReason != "" {
			retryNote = fmt.Sprintf("Reason for this run: %s. %s", p.RetryReason, baseNote)
		} else {
			retryNote = baseNote
		}
	}

	args, err := review.RenderCommand(p.CommandTemplate, review.ToolArgs{
		Repo:          p.Repo,
		PRNumber:      p.PRNumber,
		PRURL:         p.PRURL,
		Title:         title,
		BaseBranch:    base,
		Worktree:      dir,
		Guide:         guide,
		ReviewID:      p.ReviewID,
		HeadCommit:    head,
		SessionID:     p.SessionID,
		RetryNote:     retryNote,
		ResultAPIURL:  p.ResultAPIURL,
		ResultToken:   p.ResultToken,
		PriorFindings: p.PriorFindings,
	})
	if err != nil {
		w.complete(p, false, head, "", "", "render command: "+err.Error())
		return
	}

	logFile, err := openLog(w.cfg.LogDir, p.ReviewID)
	if err != nil {
		w.complete(p, false, head, "", "", "prepare log: "+err.Error())
		return
	}
	defer logFile.Close()

	jobCtx, cancel := context.WithTimeout(ctx, w.jobTimeout(p.Timeout))
	defer cancel()

	var mu sync.Mutex
	var progress string
	lastFlush := time.Now()
	sink := func(line string) {
		mu.Lock()
		progress += review.StripANSI(line)
		if len(progress) > maxProgressBytes {
			progress = progress[:maxProgressBytes]
		}
		mu.Unlock()
		_, _ = logFile.WriteString(line)
		shouldFlush := false
		mu.Lock()
		if time.Since(lastFlush) >= 5*time.Second {
			lastFlush = time.Now()
			shouldFlush = true
		}
		mu.Unlock()
		if shouldFlush {
			mu.Lock()
			snapshot := progress
			mu.Unlock()
			_ = w.send(workerproto.NewFrame(workerproto.EvProgress, p.ReviewID, workerproto.ProgressPayload{Text: snapshot}))
		}
	}

	event := w.jobEvent("tool_started", "running", p)
	event.Tool, event.LogPath = w.cfg.Tool, logFile.Name()
	event.Log()
	output, runErr := review.Run(jobCtx, w.cfg.Tool, args, dir, sink)
	if runErr != nil && jobCtx.Err() != nil {
		runErr = fmt.Errorf("%w: %v", jobCtx.Err(), runErr)
	}
	sessionCtx, scancel := context.WithTimeout(ctx, 15*time.Second)
	sessionID := review.CaptureSessionID(sessionCtx, w.cfg.Tool, dir, p.ReviewID)
	scancel()
	clean := review.StripANSI(output)
	if runErr != nil {
		w.complete(p, false, head, sessionID, clean, runErr.Error())
	} else {
		w.complete(p, true, head, sessionID, clean, "")
	}
}

func (w *Worker) runReaction(ctx context.Context, p workerproto.JobPayload) {
	dir, ok := w.cfg.Repos[p.Repo]
	if !ok {
		w.complete(p, false, "", "", "", fmt.Sprintf("repo %s is not checked out on this worker (SATURNUS_REVIEW_REPOS or SATURNUS_REVIEW_REPOS_FILE)", p.Repo))
		return
	}
	if !review.IsGitDir(dir) {
		w.complete(p, false, "", "", "", fmt.Sprintf("local checkout not found at %q", dir))
		return
	}
	if p.SessionID == "" {
		w.complete(p, false, "", "", "", "no tool session to resume; run the review first")
		return
	}
	// The instruction is passed as a single argv element so arbitrary reviewer
	// text (quotes, backslashes, whitespace) survives verbatim.
	args := []string{"run", "--session", p.SessionID, "--title", "saturnus-review-" + p.ReviewID, p.Instruction}

	logFile, err := openLog(w.cfg.LogDir, p.ReactionID)
	if err != nil {
		w.complete(p, false, "", "", "", "prepare log: "+err.Error())
		return
	}
	defer logFile.Close()

	jobCtx, cancel := context.WithTimeout(ctx, w.jobTimeout(p.Timeout))
	defer cancel()

	sink := func(line string) {
		_, _ = logFile.WriteString(line)
	}
	event := w.jobEvent("tool_started", "running", p)
	event.Tool, event.LogPath = w.cfg.Tool, logFile.Name()
	event.Log()
	output, runErr := review.Run(jobCtx, w.cfg.Tool, args, dir, sink)
	if runErr != nil && jobCtx.Err() != nil {
		runErr = fmt.Errorf("%w: %v", jobCtx.Err(), runErr)
	}
	clean := review.StripANSI(output)
	if runErr != nil {
		w.complete(p, false, "", p.SessionID, clean, runErr.Error())
	} else {
		w.complete(p, true, "", p.SessionID, clean, "")
	}
}

// maxProgressBytes bounds the accumulated streamed progress kept in memory and
// sent to the server (mirrors the local executor's bound).
const maxProgressBytes = 512 * 1024

func (w *Worker) jobTimeout(t time.Duration) time.Duration {
	if t <= 0 {
		return 15 * time.Minute
	}
	return t
}

func openLog(logDir, id string) (*os.File, error) {
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

// discoverInRepoGuide reads a repo-local review guide if present, mirroring the
// server's in-repo discovery.
func discoverInRepoGuide(dir string) string {
	for _, name := range []string{".saturnus-review.md", ".saturnus/review.md"} {
		path := filepath.Join(dir, name)
		if data, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}
