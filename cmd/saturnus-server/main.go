package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"saturnus/internal/cloudops"
	"saturnus/internal/lark"
	"saturnus/internal/policy"
	"saturnus/internal/repoconfig"
	"saturnus/internal/review"
	"saturnus/internal/store"
	"saturnus/internal/tasks"
	"saturnus/internal/worker"
	"saturnus/internal/workermonitor"
)

type server struct {
	store     *store.Store
	lark      *lark.Client
	reviews   *review.Service
	tasks     *tasks.Service
	cloudops  *cloudops.Client
	botUsers  []string
	botOpenID string
	// hub is the WebSocket control channel for remote review workers.
	hub *worker.Hub
	// workerRegisterKey authenticates worker registration when set.
	workerRegisterKey string
	// workerOnlineWindow bounds how long a worker's heartbeat stays fresh; a
	// review owned by a worker offline longer than this is requeued.
	workerOnlineWindow time.Duration
	workerAlerts       *workermonitor.Monitor
}

const shutdownTimeout = 15 * time.Second

func main() {
	addr := flag.String("addr", ":8787", "listen address")
	data := flag.String("data", "saturnus.db", "sqlite data file")
	staticDir := flag.String("static", "web/dist", "static web directory")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *addr, *data, *staticDir); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, addr, data, staticDir string) (err error) {
	repos, err := repoconfig.Load(os.Getenv("SATURNUS_REVIEW_REPOS"), os.Getenv("SATURNUS_REVIEW_REPOS_FILE"))
	if err != nil {
		return err
	}
	taskCfg, err := tasksConfig(repos)
	if err != nil {
		return err
	}
	st, err := store.Open(data)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close store: %w", closeErr))
		}
	}()
	larkClient := lark.New(lark.Config{
		AppID:     os.Getenv("LARK_APP_ID"),
		AppSecret: os.Getenv("LARK_APP_SECRET"),
	})
	cloudopsClient, err := cloudopsFromEnv()
	if err != nil {
		return err
	}
	s := &server{
		store:              st,
		lark:               larkClient,
		reviews:            review.New(st, larkClient, reviewConfig(repos)),
		cloudops:           cloudopsClient,
		botUsers:           splitCSV(os.Getenv("SATURNUS_COMMAND_ALLOWED_USERS")),
		botOpenID:          resolveBotOpenID(larkClient),
		workerRegisterKey:  os.Getenv("SATURNUS_WORKER_REGISTER_KEY"),
		workerOnlineWindow: envDuration("SATURNUS_WORKER_ONLINE_WINDOW", 90*time.Second),
	}
	taskCfg.Context = ctx
	taskCfg.SyncReview = func(ctx context.Context, repo string, num int, info tasks.PRInfo) error {
		return s.reviews.SyncPR(ctx, repo, num, review.PRInfo{Title: info.Title, BaseBranch: info.BaseBranch, HeadCommit: info.HeadCommit, State: info.State})
	}
	s.tasks = tasks.New(st, taskCfg)
	s.workerAlerts = workermonitor.New(st, larkClient, workermonitor.Config{
		ReviewerOpenID: os.Getenv("SATURNUS_REVIEW_REVIEWER_OPEN_ID"),
		ReviewerChatID: os.Getenv("SATURNUS_REVIEW_REVIEWER_CHAT_ID"),
	})

	// Control plane for remote review workers: the scheduler dispatches jobs to
	// connected workers over WebSocket, falling back to the local executor.
	sched := newScheduler(s.reviews, st)
	sched.onlineWindow = s.workerOnlineWindow
	sched.onWorkerHeartbeat = s.workerAlerts.Notify
	s.reviews.SetJobSink(sched.Notify)
	s.reviews.SetRepoProvider(sched)
	s.hub = worker.NewHub(sched, func(workerID, token string) bool {
		if workerID == "" || token == "" {
			return false
		}
		agent, err := st.GetWorker(workerID)
		return err == nil && agent.WorkerToken != "" && agent.WorkerToken == token
	})
	sched.SetHub(s.hub)
	defer s.hub.Close()

	s.reviews.Start()
	defer s.reviews.Stop()
	go sched.Run(ctx)
	stopRefresh := s.tasks.Start(ctx)
	defer stopRefresh()
	stopWorkerAlerts := s.workerAlerts.Start(ctx)
	defer stopWorkerAlerts()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/agents/register", s.registerAgent)
	mux.HandleFunc("POST /api/agents/{agent_id}/heartbeat", s.heartbeat)
	mux.HandleFunc("POST /api/workers/register", s.registerWorker)
	mux.HandleFunc("GET /api/workers/ws", s.workerWS)
	mux.HandleFunc("GET /api/workers", s.listWorkers)
	mux.HandleFunc("GET /api/sessions", s.listSessions)
	mux.HandleFunc("GET /api/sessions/{session_id}", s.getSession)
	mux.HandleFunc("PATCH /api/sessions/{session_id}/autopass", s.updateAutoPass)
	mux.HandleFunc("POST /api/sessions/{session_id}/events", s.addEvent)
	mux.HandleFunc("GET /api/approvals", s.listApprovals)
	mux.HandleFunc("POST /api/approvals", s.createApproval)
	mux.HandleFunc("GET /api/approvals/{request_id}", s.getApproval)
	mux.HandleFunc("POST /api/approvals/{request_id}/decision", s.decideApproval)
	mux.HandleFunc("GET /api/reviews", s.listReviews)
	mux.HandleFunc("GET /api/reviews/{review_id}", s.getReview)
	mux.HandleFunc("POST /api/reviews", s.createReview)
	mux.HandleFunc("POST /api/reviews/{review_id}/result", s.submitReviewResult)
	mux.HandleFunc("POST /api/reviews/{review_id}/post-comments", s.postReviewComments)
	mux.HandleFunc("GET /api/tasks", s.listTasks)
	mux.HandleFunc("GET /api/tasks/{task_id}", s.getTask)
	mux.HandleFunc("POST /api/tasks", s.createTask)
	mux.HandleFunc("POST /api/tasks/{task_id}/docs", s.addTaskDoc)
	mux.HandleFunc("DELETE /api/tasks/{task_id}/docs/{doc_id}", s.removeTaskDoc)
	mux.HandleFunc("POST /api/tasks/{task_id}/prs", s.addTaskPR)
	mux.HandleFunc("DELETE /api/tasks/{task_id}/prs/{pr_id}", s.removeTaskPR)
	mux.HandleFunc("POST /api/tasks/{task_id}/refresh", s.refreshTask)
	mux.HandleFunc("PATCH /api/tasks/{task_id}", s.updateTask)
	mux.HandleFunc("POST /lark/events", s.larkEvents)
	mux.Handle("/", spaFileServer(staticDir))

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("saturnus server listening on %s", addr)
	return serveUntilShutdown(ctx, httpServer, shutdownTimeout)
}

type gracefulHTTPServer interface {
	ListenAndServe() error
	Shutdown(context.Context) error
	Close() error
}

func serveUntilShutdown(ctx context.Context, srv gracefulHTTPServer, timeout time.Duration) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		log.Printf("shutdown signal received; allowing up to %s for active requests", timeout)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		if closeErr := srv.Close(); closeErr != nil {
			return fmt.Errorf("graceful shutdown: %w; force close: %v", err, closeErr)
		}
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve during shutdown: %w", err)
		}
		log.Printf("saturnus server stopped")
		return nil
	case <-shutdownCtx.Done():
		if err := srv.Close(); err != nil {
			return fmt.Errorf("server did not stop after shutdown: %w; force close: %v", shutdownCtx.Err(), err)
		}
		return fmt.Errorf("server did not stop after shutdown: %w", shutdownCtx.Err())
	}
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now().UTC()})
}

func (s *server) registerAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Agent   store.Agent   `json:"agent"`
		Session store.Session `json:"session"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	agent, session, err := s.store.RegisterAgentSession(req.Agent, req.Session)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": agent, "session": session})
}

func (s *server) heartbeat(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Heartbeat(r.PathValue("agent_id")); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// registerWorker registers a remote review worker: its identity (host, tool,
// version) and capabilities (repos it has checked out, gh availability,
// concurrency). When SATURNUS_WORKER_REGISTER_KEY is set the request must carry
// it; the returned agent includes the worker_token the control channel uses.
func (s *server) registerWorker(w http.ResponseWriter, r *http.Request) {
	if s.workerRegisterKey == "" {
		writeError(w, http.StatusServiceUnavailable, errors.New("worker registration is disabled (set SATURNUS_WORKER_REGISTER_KEY)"))
		return
	}
	var req struct {
		Agent        store.Agent              `json:"agent"`
		Capabilities store.WorkerCapabilities `json:"capabilities"`
		RegisterKey  string                   `json:"register_key"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RegisterKey != s.workerRegisterKey {
		writeError(w, http.StatusUnauthorized, errors.New("invalid worker register key"))
		return
	}
	if req.Agent.ID == "" {
		req.Agent.ID = "wrk_" + newToken()
	}
	agent, err := s.store.RegisterWorker(req.Agent, req.Capabilities)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if agent.WorkerToken == "" {
		token := newToken()
		if err := s.store.SetWorkerToken(agent.ID, token); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		agent.WorkerToken = token
	}
	if s.workerAlerts != nil {
		s.workerAlerts.Notify()
	}
	writeJSON(w, http.StatusOK, agent)
}

// workerWS upgrades a worker's request into its WebSocket control channel. The
// connection is authenticated by the worker_id + token query parameters.
func (s *server) workerWS(w http.ResponseWriter, r *http.Request) {
	if err := s.hub.ServeWS(w, r); err != nil {
		if r.Context().Err() == nil {
			log.Printf("worker ws: %v", err)
		}
	}
}

func (s *server) listWorkers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"workers": s.store.ListWorkers()})
}

// newToken returns a random hex token for worker authentication.
func newToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().String()))
	}
	return hex.EncodeToString(b[:])
}

func (s *server) listSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.store.ListSessions()})
}

func (s *server) getSession(w http.ResponseWriter, r *http.Request) {
	session, events, err := s.store.GetSession(r.PathValue("session_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": session, "events": events})
}

func (s *server) updateAutoPass(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool   `json:"enabled"`
		TTL     string `json:"ttl"`
		Scope   string `json:"scope"`
		Actor   string `json:"actor"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ttl := 30 * time.Minute
	if req.TTL != "" {
		parsed, err := time.ParseDuration(req.TTL)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid ttl: %w", err))
			return
		}
		ttl = parsed
	}
	if req.Actor == "" {
		req.Actor = "web"
	}
	session, err := s.store.UpdateAutoPass(r.PathValue("session_id"), req.Enabled, ttl, req.Scope, req.Actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": session})
}

func (s *server) addEvent(w http.ResponseWriter, r *http.Request) {
	var event store.Event
	if !decodeJSON(w, r, &event) {
		return
	}
	event.SessionID = r.PathValue("session_id")
	created, err := s.store.AddEvent(event)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"event": created})
}

func (s *server) listApprovals(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"approvals": s.store.ListApprovals(r.URL.Query().Get("status"))})
}

func (s *server) createApproval(w http.ResponseWriter, r *http.Request) {
	var req store.ApprovalRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	session, _, err := s.store.GetSession(req.SessionID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	eval := policy.Evaluate(session, req.ToolName, req.ToolInput)
	req.RiskLevel = eval.RiskLevel
	if eval.AutoApprove {
		req.Reason = eval.Reason
		created, err := s.store.CreateApproval(req)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		decidedReq, decision, err := s.store.DecideApproval(created.ID, store.ApprovalDecision{
			Decision:  "approved",
			DecidedBy: "policy",
			Source:    "auto",
			Reason:    eval.Reason,
		})
		if err != nil && !errors.Is(err, store.ErrAlreadyDecided) {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"approval": decidedReq, "decision": decision, "auto": true})
		return
	}
	req.Status = "pending"
	created, err := s.store.CreateApproval(req)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.notifyLarkApproval(created)
	writeJSON(w, http.StatusCreated, map[string]any{"approval": created, "auto": false, "reason": eval.Reason})
}

func (s *server) getApproval(w http.ResponseWriter, r *http.Request) {
	req, ok := s.store.GetApproval(r.PathValue("request_id"))
	if !ok {
		writeStoreError(w, store.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approval": req})
}

func (s *server) decideApproval(w http.ResponseWriter, r *http.Request) {
	var decision store.ApprovalDecision
	if !decodeJSON(w, r, &decision) {
		return
	}
	if decision.DecidedBy == "" {
		decision.DecidedBy = "web"
	}
	if decision.Source == "" {
		decision.Source = "web"
	}
	req, dec, err := s.store.DecideApproval(r.PathValue("request_id"), decision)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approval": req, "decision": dec})
}

func (s *server) listReviews(w http.ResponseWriter, r *http.Request) {
	includeArchived := r.URL.Query().Get("archived") == "1" || r.URL.Query().Get("archived") == "true"
	writeJSON(w, http.StatusOK, map[string]any{"reviews": s.reviews.List(r.URL.Query().Get("status"), includeArchived)})
}

func (s *server) getReview(w http.ResponseWriter, r *http.Request) {
	req, err := s.reviews.Get(r.PathValue("review_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.reviews.ResolveRequesterName(&req); err != nil {
		logRequesterResolveFailure(req, err)
	}
	contextID, err := s.reviews.ContextID(req.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"review": req, "context_id": contextID, "runs": s.reviews.ListRuns(req.ID), "reactions": s.reviews.ListReactions(req.ID), "threads": s.reviews.ListThreads(req.ID), "findings": s.reviews.ListFindings(req.ID)})
}

// submitReviewResult receives the machine-readable review result the review
// tool POSTs while a review runs (the endpoint is advertised in the tool's
// prompt). When SATURNUS_REVIEW_RESULT_TOKEN is set, the request must carry
// it as "Authorization: Bearer <token>". The body is validated and persisted
// as the review's verdict, summary and findings.
func (s *server) submitReviewResult(w http.ResponseWriter, r *http.Request) {
	if s.reviews == nil || !s.reviews.Enabled() {
		writeError(w, http.StatusServiceUnavailable, errors.New("review service is disabled"))
		return
	}
	if token := s.reviews.ResultToken(); token != "" {
		if !bearerMatches(r, token) {
			writeError(w, http.StatusUnauthorized, errors.New("invalid or missing review result token"))
			return
		}
	}
	var body struct {
		Verdict  string           `json:"verdict"`
		Summary  string           `json:"summary"`
		Findings []review.Finding `json:"findings"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.reviews.IngestResult(r.PathValue("review_id"), body.Verdict, body.Summary, body.Findings); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeStoreError(w, err)
		case strings.Contains(err.Error(), "not running"), strings.Contains(err.Error(), "already has a structured result"):
			writeError(w, http.StatusConflict, err)
		default:
			writeError(w, http.StatusBadRequest, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// postReviewComments manually publishes a review's findings (and verdict
// review) to the PR as GitHub inline comments. It is the recovery path for
// reviews whose findings were never posted when they finished.
func (s *server) postReviewComments(w http.ResponseWriter, r *http.Request) {
	if s.reviews == nil || !s.reviews.Enabled() {
		writeError(w, http.StatusServiceUnavailable, errors.New("review service is disabled"))
		return
	}
	posted, err := s.reviews.PostComments(r.PathValue("review_id"))
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeStoreError(w, err)
		case errors.Is(err, review.ErrArchived):
			writeError(w, http.StatusConflict, err)
		case strings.Contains(err.Error(), "already posted"):
			writeError(w, http.StatusConflict, err)
		default:
			writeError(w, http.StatusBadRequest, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "posted": posted})
}

// bearerMatches reports whether the request's Authorization header carries the
// expected bearer token.
func bearerMatches(r *http.Request, token string) bool {
	auth := r.Header.Get("Authorization")
	return auth == "Bearer "+token
}

func (s *server) createReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PRURL         string `json:"pr_url"`
		ChatID        string `json:"chat_id"`
		MessageID     string `json:"message_id"`
		RequesterID   string `json:"requester_open_id"`
		RequesterName string `json:"requester_name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RequesterID == "" {
		req.RequesterID = "api"
	}
	created, err := s.reviews.Submit(review.MessageContext{
		Text:          req.PRURL,
		ChatID:        req.ChatID,
		MessageID:     req.MessageID,
		SenderOpenID:  req.RequesterID,
		SenderName:    req.RequesterName,
		RootMessageID: req.MessageID,
	})
	if err != nil {
		if errors.Is(err, review.ErrAlreadyPending) {
			writeJSON(w, http.StatusOK, map[string]any{"review": created, "reused": true})
			return
		}
		if errors.Is(err, review.ErrReviewUnchanged) {
			writeJSON(w, http.StatusOK, map[string]any{"review": created, "reused": true, "unchanged": true})
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"review": created})
}

func (s *server) listTasks(w http.ResponseWriter, r *http.Request) {
	includeArchived := r.URL.Query().Get("archived") == "1" || r.URL.Query().Get("archived") == "true"
	writeJSON(w, http.StatusOK, map[string]any{"tasks": s.tasks.List(r.URL.Query().Get("status"), includeArchived)})
}

func (s *server) getTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.tasks.Get(r.PathValue("task_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"task": task,
		"docs": s.tasks.Docs(task.ID),
		"prs":  s.tasks.PRs(task.ID),
	})
}

func (s *server) createTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IssueURL string `json:"issue_url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.IssueURL == "" {
		writeError(w, http.StatusBadRequest, errors.New("issue_url is required"))
		return
	}
	task, err := s.tasks.Create(req.IssueURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"task": task})
}

func (s *server) updateTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.tasks.Get(r.PathValue("task_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var req struct {
		Title       *string `json:"title"`
		Status      *string `json:"status"`
		Description *string `json:"description"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Status != nil {
		if *req.Status == store.TaskStatusArchived {
			updated, err := s.tasks.Close(task.ID)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"task": updated})
			return
		}
	}
	updated, err := s.store.PatchTask(task.ID, req.Title, req.Status, req.Description)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": updated})
}

func (s *server) addTaskDoc(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo string `json:"repo"`
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Repo == "" || req.Path == "" {
		writeError(w, http.StatusBadRequest, errors.New("repo and path are required"))
		return
	}
	doc, err := s.tasks.AddDoc(r.PathValue("task_id"), tasks.FormatRepoPath(req.Repo, req.Path))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"doc": doc})
}

func (s *server) removeTaskDoc(w http.ResponseWriter, r *http.Request) {
	if err := s.tasks.RemoveDoc(r.PathValue("doc_id")); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) addTaskPR(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PRURL string `json:"pr_url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.PRURL == "" {
		writeError(w, http.StatusBadRequest, errors.New("pr_url is required"))
		return
	}
	pr, err := s.tasks.AddPR(r.PathValue("task_id"), req.PRURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"pr": pr})
}

func (s *server) removeTaskPR(w http.ResponseWriter, r *http.Request) {
	if err := s.tasks.RemovePR(r.PathValue("pr_id")); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) refreshTask(w http.ResponseWriter, r *http.Request) {
	result, err := s.tasks.RefreshContext(r.Context(), r.PathValue("task_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) larkEvents(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	if !decodeJSON(w, r, &payload) {
		return
	}
	if challenge, ok := payload["challenge"].(string); ok {
		writeJSON(w, http.StatusOK, map[string]string{"challenge": challenge})
		return
	}
	msg := extractLarkMessage(payload)
	if !s.shouldRespond(msg) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	command := satCommand(msg.Text)
	if command == "" {
		if s.handleReviewThreadReply(msg) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		if reply := s.maybeReview(msg); reply != "" {
			_ = s.lark.SendText("chat_id", msg.ChatID, reply)
		} else if !s.botAllowed(msg.SenderOpenID) {
			_ = s.lark.SendText("chat_id", msg.ChatID, s.reviewUsageExample())
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if !s.botAllowed(msg.SenderOpenID) {
		_ = s.lark.SendText("chat_id", msg.ChatID, s.reviewUsageExample())
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	// ACK first: some commands (e.g. /sat refresh all) can take minutes, and
	// without an immediate reply the sender cannot tell whether the server
	// received the command or is just down. The ACK text message is then
	// rewritten in place with the final result (Feishu allows editing text
	// messages), or marked done when the reply is delivered as a card.
	ackID, ackErr := s.lark.SendTextGetID("chat_id", msg.ChatID, ackText(command))
	reply := s.handleBotCommand(command, msg)
	switch {
	case ackErr != nil || ackID == "":
		// ACK could not be sent; fall back to the plain reply path.
		s.sendReply(msg, reply)
	case reply.table == nil && reply.md == nil:
		// Plain-text result: turn the ACK into the answer. If the in-place
		// update fails, send the reply as a fresh message instead.
		if reply.text != "" && s.lark.UpdateMessage(ackID, reply.text) == nil {
			break
		}
		_ = s.lark.UpdateMessage(ackID, ackDoneText(command))
		if reply.text != "" {
			s.sendReply(msg, reply)
		}
	default:
		// Card result: mark the ACK as done and deliver the card separately.
		_ = s.lark.UpdateMessage(ackID, ackDoneText(command))
		if !s.sendCard(msg, reply) {
			s.sendReply(msg, reply)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reply": reply.text})
}

func (s *server) botAllowed(openID string) bool {
	if openID == "" {
		return false
	}
	for _, user := range s.botUsers {
		if user == "*" {
			return true
		}
		if user == openID {
			return true
		}
	}
	return false
}

// shouldRespond reports whether a received Lark message should be handled.
// Direct (p2p) messages are always handled. In a group the behavior depends on
// the chat's mode flags (default review):
//
//   - review: respond only when the bot is mentioned directly by its own
//     open_id; "@all" mentions (id "all") and mentions of other users do not
//     count.
//   - report: the chat is a quiet report channel — all incoming messages are
//     ignored.
//   - report|review: quiet except that review requests are still acted on.
func (s *server) shouldRespond(msg review.MessageContext) bool {
	if msg.ChatType != "group" {
		return true
	}
	flags := store.ChatModeFlags(s.store.GetChatMode(msg.ChatID))
	if flags[store.ChatModeReport] {
		return flags[store.ChatModeReview] && isReviewRequest(msg.Text) && s.botMentioned(msg)
	}
	return s.botMentioned(msg)
}

// botMentioned reports whether the message directly mentions the bot by its
// own open_id.
func (s *server) botMentioned(msg review.MessageContext) bool {
	if s.botOpenID == "" {
		log.Printf("lark: group message received but bot open_id is unknown; ignoring (set SATURNUS_LARK_BOT_OPEN_ID)")
		return false
	}
	for _, id := range msg.Mentions {
		if id == s.botOpenID {
			return true
		}
	}
	return false
}

// isReviewRequest reports whether a group message asks for review work: it
// contains a GitHub PR URL, or is a /sat command from the review lifecycle.
func isReviewRequest(text string) bool {
	if _, err := review.FindPRRequest(text); err == nil {
		return true
	}
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) >= 2 && fields[0] == "/sat" {
		switch fields[1] {
		case "review", "retry", "reset", "refresh", "act", "threads", "describe", "reply", "post":
			return true
		}
	}
	return false
}

// attributedRequester returns the open_id of the first @mentioned user other
// than the bot (and other than the "@all" pseudo-mention). This lets an
// admin attribute a review's requester to a specific user, e.g.
// "/sat review <pr-url> @user". Empty when only the bot (or nobody) was
// mentioned.
func (s *server) attributedRequester(msg review.MessageContext) string {
	openID, _ := s.mentionedUser(msg)
	return openID
}

// mentionedUser returns the open_id and display name of the first @mentioned
// user other than the bot (and other than "@all").
func (s *server) mentionedUser(msg review.MessageContext) (string, string) {
	for _, id := range msg.Mentions {
		if id == "" || id == s.botOpenID || id == "all" {
			continue
		}
		return id, msg.MentionNames[id]
	}
	return "", ""
}

// requesterReply handles /sat requester <review_id> @user|ou_xxx, which
// reassigns the requester of an existing review.
func (s *server) requesterReply(fields []string, msg review.MessageContext) botReply {
	if len(fields) < 3 {
		return botReply{text: "Usage: /sat requester <review_id> @user or /sat requester <review_id> ou_xxx"}
	}
	openID, name := s.mentionedUser(msg)
	if openID == "" && len(fields) >= 4 && strings.HasPrefix(fields[3], "ou_") {
		openID, name = fields[3], ""
	}
	if openID == "" {
		return botReply{text: "No user specified: mention a user in the command, or pass an open_id (/sat requester <review_id> ou_xxx)."}
	}
	req, err := s.reviews.SetRequester(fields[2], openID, name)
	if err != nil {
		if errors.Is(err, review.ErrArchived) {
			return botReply{text: "Review " + fields[2] + " is archived and cannot be reassigned."}
		}
		return botReply{text: "Failed to update requester: " + err.Error()}
	}
	return botReply{text: fmt.Sprintf("Review %s requester updated to %s (%s)", req.ID, req.RequesterName, req.RequesterOpenID)}
}

// resolveBotOpenID determines the bot's own open_id, used to detect direct
// group mentions. An explicit SATURNUS_LARK_BOT_OPEN_ID wins; otherwise the
// app bot identity is looked up via bot/v3/info. Failures are logged and the
// result may be empty, in which case group messages are ignored.
func resolveBotOpenID(larkClient *lark.Client) string {
	if value := os.Getenv("SATURNUS_LARK_BOT_OPEN_ID"); value != "" {
		return value
	}
	if !larkClient.Enabled() {
		return ""
	}
	info, err := larkClient.BotInfo()
	if err != nil {
		log.Printf("lark: could not resolve bot open_id (set SATURNUS_LARK_BOT_OPEN_ID to override): %v", err)
		return ""
	}
	if info.OpenID == "" {
		log.Printf("lark: bot info returned no open_id (set SATURNUS_LARK_BOT_OPEN_ID)")
		return ""
	}
	log.Printf("lark: resolved bot open_id=%s (app=%s)", info.OpenID, info.Name)
	return info.OpenID
}

func (s *server) reviewUsageExample() string {
	var b strings.Builder
	b.WriteString("你好，我可以发起代码评审。直接发送 GitHub PR 链接即可，例如：")
	b.WriteString("\nreview https://github.com/congqixia/saturnus/pull/1")
	b.WriteString("\n或直接把 PR 链接粘贴过来，我会安排评审并把结果通知评审人。")
	if s.reviews != nil && s.reviews.Enabled() {
		repos := s.reviews.FormatRepos()
		if repos != "" && !strings.HasPrefix(repos, "No repos") {
			fmt.Fprintf(&b, "\n\n当前可评审的仓库：\n%s", repos)
		}
	}
	return b.String()
}

func (s *server) maybeReview(msg review.MessageContext) string {
	if !s.reviews.Enabled() {
		return ""
	}
	if _, err := review.FindPRRequest(msg.Text); err != nil {
		return ""
	}
	// Natural-language requests keep the sender as the requester. Any @user in
	// the message is usually the reviewer being asked to approve, so mentions
	// must NOT be interpreted as requester here; only the explicit
	// "/sat review <pr-url> @user" command attributes the requester.
	req, err := s.reviews.SubmitPublic(msg)
	if err != nil {
		if errors.Is(err, review.ErrAlreadyPending) {
			return "Review already in progress: " + req.ID + " (" + req.PRURL + ")"
		}
		if errors.Is(err, review.ErrReviewUnchanged) {
			return "Review " + req.ID + " is already up to date: PR head commit unchanged since the last review."
		}
		return "Review failed: " + err.Error()
	}
	return "Review started " + req.ID + " (" + req.PRURL + ")"
}

// handleReviewThreadReply implements the "reply in a review thread" trigger: a
// group message that is a direct bot mention inside a review thread re-checks
// whether the PR head moved and, when it did, re-runs the review (notifying the
// requester in-thread and the reviewer). Replies that are /sat commands or
// contain a PR URL are left to the normal paths. It returns true when the
// message was consumed by this trigger.
func (s *server) handleReviewThreadReply(msg review.MessageContext) bool {
	if !s.reviews.Enabled() || msg.ChatType != "group" {
		return false
	}
	if msg.RootMessageID == "" || msg.RootMessageID == msg.MessageID {
		return false // not a reply in a thread
	}
	if _, err := review.FindPRRequest(msg.Text); err == nil {
		return false // explicit PR URL is handled by the normal review path
	}
	req, retried, err := s.reviews.RetryOnReply(msg)
	if err != nil {
		if errors.Is(err, review.ErrArchived) {
			s.replyInThread(msg, "该 review 已归档，无法重新评审。")
			return true
		}
		return false // no active review in this thread; fall through
	}
	if retried {
		return true // requester and reviewer already notified by the service
	}
	if req.Status == "pending" || req.Status == "reviewing" {
		s.replyInThread(msg, fmt.Sprintf("Review %s 正在运行中，无需重复触发。", req.ID))
	} else {
		s.replyInThread(msg, fmt.Sprintf("PR head 未变更，Review %s 已是最新，无需重新评审。", req.ID))
	}
	return true
}

// replyInThread replies to a message in its thread.
func (s *server) replyInThread(msg review.MessageContext, text string) {
	if msg.MessageID == "" {
		return
	}
	if err := s.lark.ReplyMessage(msg.MessageID, text); err != nil {
		log.Printf("reply in thread: %v", err)
	}
}

type botReply struct {
	text  string
	table *lark.TableCard
	md    *lark.MarkdownCard
}

func (s *server) sendReply(msg review.MessageContext, reply botReply) {
	if msg.ChatID == "" {
		return
	}
	switch {
	case reply.table != nil:
		if err := s.lark.SendTable("chat_id", msg.ChatID, *reply.table); err == nil {
			return
		} else {
			log.Printf("send card to %s: %v", msg.ChatID, err)
		}
	case reply.md != nil:
		if err := s.lark.SendMarkdownCard("chat_id", msg.ChatID, *reply.md); err == nil {
			return
		} else {
			log.Printf("send card to %s: %v", msg.ChatID, err)
		}
	}
	if reply.text != "" {
		if err := s.lark.SendText("chat_id", msg.ChatID, reply.text); err != nil {
			log.Printf("send text to %s: %v", msg.ChatID, err)
		}
	}
}

// sendCard delivers only the card part of a reply (table or markdown), falling
// back to nothing. It reports whether the card was sent, so the caller can
// decide whether a text fallback is still needed.
func (s *server) sendCard(msg review.MessageContext, reply botReply) bool {
	if msg.ChatID == "" {
		return false
	}
	if reply.table != nil {
		if err := s.lark.SendTable("chat_id", msg.ChatID, *reply.table); err == nil {
			return true
		} else {
			log.Printf("send card to %s: %v", msg.ChatID, err)
		}
	}
	if reply.md != nil {
		if err := s.lark.SendMarkdownCard("chat_id", msg.ChatID, *reply.md); err == nil {
			return true
		} else {
			log.Printf("send card to %s: %v", msg.ChatID, err)
		}
	}
	return false
}

// ackText is the immediate "command received, executing" message sent before a
// /sat command runs. It is later rewritten in place with the result.
func ackText(command string) string {
	return "Command received, processing: " + command + "\nI'll update this message with the result when it's done."
}

// ackDoneText is the summary the ACK message is rewritten to when the reply is
// delivered as a card (so the card, not the ACK, carries the details).
func ackDoneText(command string) string {
	return "Done: " + command
}

func optionTag(text, color string) map[string]any {
	if color == "" {
		return map[string]any{"text": text}
	}
	return map[string]any{"text": text, "color": color}
}

func statusColor(status string) string {
	switch status {
	case "succeeded":
		return "green"
	case "failed":
		return "red"
	case "pending":
		return "blue"
	case "reviewing":
		return "orange"
	case "archived":
		return "grey"
	}
	return ""
}

func statusOption(status string) any {
	return optionTag(status, statusColor(status))
}

// reviewVerdictTag renders a structured review verdict as a colored tag for
// the describe card: lgtm green, request_change red, nit_comments grey.
func reviewVerdictTag(verdict string) string {
	label := map[string]string{
		"lgtm":           "LGTM",
		"request_change": "Request change",
		"nit_comments":   "Nit comments",
	}[verdict]
	if label == "" {
		label = verdict
	}
	return "`" + label + "`"
}

func riskOption(risk string) any {
	color := ""
	switch strings.ToLower(risk) {
	case "high":
		color = "red"
	case "medium":
		color = "orange"
	case "low":
		color = "green"
	}
	return optionTag(risk, color)
}

func boolOption(v bool) any {
	if v {
		return optionTag("true", "green")
	}
	return optionTag("false", "grey")
}

// The /sat query commands below (sessions/pending/whois/reviews) render results
// as native Feishu card-2.0 tables. The card/table schema, doc links and the
// sizing rules are documented in internal/lark/table.go. Conventions used here:
//   - each table uses percentage column widths summing to 100% so it fills the
//     card (width_mode "fill") without horizontal overflow;
//   - status/risk/autopass cells use colored "options" tags (optionTag);
//   - timestamps use the native "date" column type (Unix ms) so Feishu renders
//     them in the reader's local timezone;
//   - at most 10 rows are shown (matches the table page_size of 10); the plain
//     text field is kept as a fallback when card sending fails.

func (s *server) sessionsReply() botReply {
	sessions := s.store.ListSessions()
	if len(sessions) == 0 {
		return botReply{text: "No sessions."}
	}
	if len(sessions) > 10 {
		sessions = sessions[:10]
	}
	var b strings.Builder
	rows := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		fmt.Fprintf(&b, "%s %s auto=%v updated=%s\n", session.ID, session.Status, session.AutoPass, session.UpdatedAt.Format(time.RFC3339))
		rows = append(rows, map[string]any{
			"id":        session.ID,
			"status":    statusOption(session.Status),
			"autopass":  boolOption(session.AutoPass),
			"updatedAt": lark.UnixMillis(session.UpdatedAt),
		})
	}
	return botReply{
		text: strings.TrimSpace(b.String()),
		table: &lark.TableCard{
			Title: "Sessions",
			Columns: []lark.TableColumn{
				{Name: "id", Display: "ID", DataType: lark.ColumnText, Width: "35%"},
				{Name: "status", Display: "Status", DataType: lark.ColumnOption, Width: "18%"},
				{Name: "autopass", Display: "AutoPass", DataType: lark.ColumnOption, Width: "18%"},
				{Name: "updatedAt", Display: "UpdatedAt", DataType: lark.ColumnDate, DateFormat: "YYYY-MM-DD HH:mm", Width: "29%"},
			},
			Rows: rows,
		},
	}
}

func (s *server) pendingReply() botReply {
	approvals := s.store.ListApprovals("pending")
	if len(approvals) == 0 {
		return botReply{text: "No pending approvals."}
	}
	if len(approvals) > 10 {
		approvals = approvals[:10]
	}
	var b strings.Builder
	rows := make([]map[string]any, 0, len(approvals))
	for _, req := range approvals {
		fmt.Fprintf(&b, "%s session=%s tool=%s risk=%s\n", req.ID, req.SessionID, req.ToolName, req.RiskLevel)
		rows = append(rows, map[string]any{
			"id":      req.ID,
			"session": req.SessionID,
			"tool":    req.ToolName,
			"risk":    riskOption(req.RiskLevel),
		})
	}
	return botReply{
		text: strings.TrimSpace(b.String()),
		table: &lark.TableCard{
			Title: "Pending Approvals",
			Columns: []lark.TableColumn{
				{Name: "id", Display: "ID", DataType: lark.ColumnText, Width: "30%"},
				{Name: "session", Display: "Session", DataType: lark.ColumnText, Width: "40%"},
				{Name: "tool", Display: "Tool", DataType: lark.ColumnText, Width: "12%"},
				{Name: "risk", Display: "Risk", DataType: lark.ColumnOption, Width: "18%"},
			},
			Rows: rows,
		},
	}
}

func (s *server) whoisReply(query string) botReply {
	users, err := s.lark.ResolveUser(query)
	if err != nil {
		return botReply{text: "Whois failed: " + err.Error()}
	}
	if len(users) == 0 {
		return botReply{text: "No users found for " + query}
	}
	var b strings.Builder
	rows := make([]map[string]any, 0, len(users))
	for _, user := range users {
		fmt.Fprintf(&b, "%s open_id=%s", user.Name, user.OpenID)
		if user.Email != "" {
			fmt.Fprintf(&b, " email=%s", user.Email)
		}
		if user.Mobile != "" {
			fmt.Fprintf(&b, " mobile=%s", user.Mobile)
		}
		b.WriteString("\n")
		rows = append(rows, map[string]any{
			"name":   user.Name,
			"openID": user.OpenID,
			"email":  user.Email,
			"mobile": user.Mobile,
		})
	}
	return botReply{
		text: strings.TrimSpace(b.String()),
		table: &lark.TableCard{
			Title: "Users",
			Columns: []lark.TableColumn{
				{Name: "name", Display: "Name", DataType: lark.ColumnText, Width: "15%"},
				{Name: "openID", Display: "OpenID", DataType: lark.ColumnText, Width: "35%"},
				{Name: "email", Display: "Email", DataType: lark.ColumnText, Width: "30%"},
				{Name: "mobile", Display: "Mobile", DataType: lark.ColumnText, Width: "20%"},
			},
			Rows: rows,
		},
	}
}

// requesterNameResolveBudget bounds how long a /sat query spends resolving
// requester names so a slow or down contact API cannot stall the reply.
const requesterNameResolveBudget = 3 * time.Second

// logRequesterResolveFailure logs a failed requester-name lookup with enough
// context (review id, open_id, status) to analyze later without blocking.
func logRequesterResolveFailure(req store.ReviewRequest, err error) {
	if err == nil {
		return
	}
	log.Printf("saturnus: resolve requester name failed for review %s (open_id=%s status=%s): %v", req.ID, req.RequesterOpenID, req.Status, err)
}

// resolveRequesterNames fills missing requester names for a review list,
// persisting successful lookups. It is best-effort and non-blocking: when the
// contact API fails or the time budget is exhausted, the caller keeps showing
// the open_id and the failure is logged server-side for later analysis.
func (s *server) resolveRequesterNames(reqs []store.ReviewRequest) {
	deadline := time.Now().Add(requesterNameResolveBudget)
	skipped := 0
	for i := range reqs {
		req := &reqs[i]
		if req.RequesterName != "" || req.RequesterOpenID == "" {
			continue
		}
		if time.Now().After(deadline) {
			skipped++
			continue
		}
		if err := s.reviews.ResolveRequesterName(req); err != nil {
			logRequesterResolveFailure(*req, err)
		}
	}
	if skipped > 0 {
		log.Printf("saturnus: requester name resolution budget exceeded; skipped %d review(s), open_id shown", skipped)
	}
}

// reviewsReply renders the review list. mode is "" (default, excludes archived),
// "all" (includes archived), or "archived" (only archived).
func (s *server) reviewsReply(mode string) botReply {
	includeArchived := mode == "all" || mode == "archived"
	reqs := s.reviews.List("", includeArchived)
	if mode == "archived" {
		reqs = filterStatus(reqs, "archived")
	}
	if len(reqs) == 0 {
		switch mode {
		case "archived":
			return botReply{text: "No archived reviews."}
		case "all":
			return botReply{text: "No reviews."}
		default:
			return botReply{text: "No active reviews."}
		}
	}
	if len(reqs) > 10 {
		reqs = reqs[:10]
	}
	// Best-effort: fill missing requester names (persisted on success) and log
	// failures. Never blocks the reply; unresolved names fall back to open_id.
	s.resolveRequesterNames(reqs)

	rows := make([]map[string]any, 0, len(reqs))
	for _, req := range reqs {
		rows = append(rows, map[string]any{
			"id":        req.ID,
			"status":    reviewStatusCell(req),
			"worker":    review.WorkerLabel(req.Status, req.WorkerID),
			"repo":      fmt.Sprintf("%s#%d", req.Repo, req.PRNumber),
			"requester": requesterDisplayName(req),
			"createdAt": lark.UnixMillis(req.CreatedAt),
		})
	}
	title := "PR Reviews"
	if mode == "archived" {
		title = "Archived PR Reviews"
	}
	return botReply{
		text: s.reviews.FormatList(reqs),
		table: &lark.TableCard{
			Title: title,
			Columns: []lark.TableColumn{
				{Name: "id", Display: "ID", DataType: lark.ColumnText, Width: "17%"},
				{Name: "status", Display: "Status", DataType: lark.ColumnOption, Width: "14%"},
				{Name: "repo", Display: "Repo#PR", DataType: lark.ColumnText, Width: "20%"},
				{Name: "worker", Display: "Worker", DataType: lark.ColumnText, Width: "20%"},
				{Name: "requester", Display: "Requester", DataType: lark.ColumnText, Width: "16%"},
				{Name: "createdAt", Display: "Created", DataType: lark.ColumnDate, DateFormat: "YYYY-MM-DD HH:mm", Width: "13%"},
			},
			Rows: rows,
		},
	}
}

// tasksReply renders the task list. mode is "" (default, excludes archived),
// "all" (includes archived), or "archived" (only archived).
func (s *server) tasksReply(mode string) botReply {
	includeArchived := mode == "all" || mode == "archived"
	tasks := s.tasks.List("", includeArchived)
	if mode == "archived" {
		tasks = filterTasks(tasks, store.TaskStatusArchived)
	}
	if len(tasks) == 0 {
		switch mode {
		case "archived":
			return botReply{text: "No archived tasks."}
		case "all":
			return botReply{text: "No tasks."}
		default:
			return botReply{text: "No active tasks."}
		}
	}
	if len(tasks) > 10 {
		tasks = tasks[:10]
	}
	rows := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		rows = append(rows, map[string]any{
			"id":        task.ID,
			"status":    taskStatusOption(task.Status),
			"title":     task.Title,
			"issue":     issueRef(task),
			"prs":       len(s.tasks.PRs(task.ID)),
			"createdAt": lark.UnixMillis(task.CreatedAt),
		})
	}
	title := "Tasks"
	if mode == "archived" {
		title = "Archived Tasks"
	}
	return botReply{
		text: s.tasks.FormatList(tasks),
		table: &lark.TableCard{
			Title: title,
			Columns: []lark.TableColumn{
				{Name: "id", Display: "ID", DataType: lark.ColumnText, Width: "18%"},
				{Name: "status", Display: "Status", DataType: lark.ColumnOption, Width: "14%"},
				{Name: "title", Display: "Title", DataType: lark.ColumnText, Width: "34%"},
				{Name: "issue", Display: "Issue", DataType: lark.ColumnText, Width: "16%"},
				{Name: "prs", Display: "PRs", DataType: lark.ColumnNumber, Width: "8%"},
				{Name: "createdAt", Display: "Created", DataType: lark.ColumnDate, DateFormat: "YYYY-MM-DD HH:mm", Width: "10%"},
			},
			Rows: rows,
		},
	}
}

// filterTasks keeps only tasks whose status equals want.
func filterTasks(tasks []store.Task, want string) []store.Task {
	out := make([]store.Task, 0, len(tasks))
	for _, task := range tasks {
		if task.Status == want {
			out = append(out, task)
		}
	}
	return out
}

func taskStatusColor(status string) string {
	switch status {
	case store.TaskStatusOpen:
		return "blue"
	case store.TaskStatusInProgress:
		return "orange"
	case store.TaskStatusDone:
		return "green"
	case store.TaskStatusArchived:
		return "grey"
	}
	return ""
}

func taskStatusOption(status string) any {
	return optionTag(status, taskStatusColor(status))
}

// issueRef renders the bound issue as repo#number, or "-" when none is bound.
func issueRef(task store.Task) string {
	if task.IssueRepo == "" {
		return "-"
	}
	return fmt.Sprintf("%s#%d", task.IssueRepo, task.IssueNumber)
}

// taskCommand dispatches the /sat task subcommands.
func (s *server) taskCommand(fields []string, text string) botReply {
	if len(fields) < 3 {
		return botReply{text: "Usage: /sat task <task_id> or /sat task <create|add-doc|rm-doc|add-pr|rm-pr|refresh|close> ..."}
	}
	sub := fields[2]
	switch sub {
	case "create":
		if len(fields) < 4 {
			return botReply{text: "Usage: /sat task create <issue-url>"}
		}
		issueURL := strings.TrimSpace(strings.TrimPrefix(text, strings.Join(fields[:3], " ")))
		if issueURL == "" {
			return botReply{text: "Usage: /sat task create <issue-url>"}
		}
		task, err := s.tasks.Create(issueURL)
		if err != nil {
			return botReply{text: "Task create failed: " + err.Error()}
		}
		return botReply{text: "Task created " + task.ID + " (" + task.Title + ")"}
	case "add-doc":
		if len(fields) < 5 {
			return botReply{text: "Usage: /sat task add-doc <task_id> <owner/repo:path>"}
		}
		doc, err := s.tasks.AddDoc(fields[3], fields[4])
		if err != nil {
			return botReply{text: "Add doc failed: " + err.Error()}
		}
		return botReply{text: "Doc added " + doc.ID + " (" + tasks.FormatRepoPath(doc.Repo, doc.Path) + ")"}
	case "rm-doc":
		if len(fields) < 4 {
			return botReply{text: "Usage: /sat task rm-doc <task_id> <doc_id>"}
		}
		if err := s.tasks.RemoveDoc(fields[3]); err != nil {
			return botReply{text: "Remove doc failed: " + err.Error()}
		}
		return botReply{text: "Doc removed " + fields[3]}
	case "add-pr":
		if len(fields) < 5 {
			return botReply{text: "Usage: /sat task add-pr <task_id> <pr-url>"}
		}
		pr, err := s.tasks.AddPR(fields[3], fields[4])
		if err != nil {
			return botReply{text: "Add PR failed: " + err.Error()}
		}
		return botReply{text: "PR added " + pr.ID + " (" + pr.PRURL + ") review=" + pr.ReviewStatus}
	case "rm-pr":
		if len(fields) < 4 {
			return botReply{text: "Usage: /sat task rm-pr <task_id> <pr_id>"}
		}
		if err := s.tasks.RemovePR(fields[3]); err != nil {
			return botReply{text: "Remove PR failed: " + err.Error()}
		}
		return botReply{text: "PR removed " + fields[3]}
	case "refresh":
		if len(fields) < 4 {
			return botReply{text: "Usage: /sat task refresh <task_id|all>"}
		}
		return s.taskRefreshReply(fields[3])
	case "close":
		if len(fields) < 4 {
			return botReply{text: "Usage: /sat task close <task_id>"}
		}
		task, err := s.tasks.Close(fields[3])
		if err != nil {
			return botReply{text: "Close failed: " + err.Error()}
		}
		return botReply{text: "Task closed " + task.ID}
	default:
		task, err := s.tasks.Get(sub)
		if err != nil {
			return botReply{text: "Task not found. Usage: /sat task <task_id> or /sat task <create|add-doc|rm-doc|add-pr|rm-pr|refresh|close> ..."}
		}
		return s.taskDetailReply(task)
	}
}

func (s *server) taskDetailReply(task store.Task) botReply {
	docs := s.tasks.Docs(task.ID)
	prs := s.tasks.PRs(task.ID)
	return botReply{
		text: s.tasks.FormatOne(task, docs, prs),
		md: &lark.MarkdownCard{
			Title:   "Task " + task.ID,
			Content: formatTaskMarkdown(task, docs, prs),
		},
	}
}

// formatTaskMarkdown renders a single task as markdown for a card, mirroring
// the review describe view: metadata, bound issue, docs, and per-PR progress.
// Metadata values are escaped so they cannot break the markdown.
func formatTaskMarkdown(task store.Task, docs []store.TaskDoc, prs []store.TaskPR) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Status**: `%s`\n", task.Status)
	if task.IssueURL != "" {
		label := issueRef(task)
		if label == "-" {
			label = task.IssueURL
		}
		fmt.Fprintf(&b, "**Issue**: [%s](%s)", escapeMarkdown(label), task.IssueURL)
		if task.IssueState != "" {
			fmt.Fprintf(&b, " (%s)", task.IssueState)
		}
		b.WriteString("\n")
	}
	if !task.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "**Created**: %s\n", task.CreatedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if !task.UpdatedAt.IsZero() {
		fmt.Fprintf(&b, "**Updated**: %s\n", task.UpdatedAt.Local().Format("2006-01-02 15:04:05"))
	}
	b.WriteString(formatTaskFreshness(task))
	if suggestion := tasks.CloseSuggestion(task, prs); suggestion != "" {
		fmt.Fprintf(&b, "%s\n", escapeMarkdown(suggestion))
	}
	if !task.ArchivedAt.IsZero() {
		fmt.Fprintf(&b, "**Archived**: %s\n", task.ArchivedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if len(docs) > 0 {
		b.WriteString("\n**Docs**:\n")
		for _, doc := range docs {
			fmt.Fprintf(&b, "- `%s` %s\n", doc.ID, escapeMarkdown(tasks.FormatRepoPath(doc.Repo, doc.Path)))
		}
	}
	if len(prs) > 0 {
		b.WriteString("\n**PRs**:\n")
		b.WriteString(formatTaskPRTable(prs))
	}
	if task.Description != "" {
		fmt.Fprintf(&b, "\n**Description**: %s\n", escapeMarkdown(truncateText(task.Description, maxSummaryChars)))
	}
	return strings.TrimSpace(b.String())
}

func formatTaskFreshness(task store.Task) string {
	var b strings.Builder
	if task.RefreshedAt.IsZero() {
		b.WriteString("**Last successful refresh**: never\n")
	} else {
		fmt.Fprintf(&b, "**Last successful refresh**: %s\n", task.RefreshedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if task.RefreshError != "" {
		fmt.Fprintf(&b, "**Refresh error**: %s\n", escapeMarkdown(task.RefreshError))
	}
	return b.String()
}

// formatTaskPRTable renders the per-PR progress of a task as a markdown table.
// PR and Feishu-task references become links; values are escaped so they cannot
// break the table.
func formatTaskPRTable(prs []store.TaskPR) string {
	var b strings.Builder
	b.WriteString("| PR | Title | State | Review | Task |\n")
	b.WriteString("| -- | ----- | ----- | ------ | ---- |\n")
	for _, pr := range prs {
		label := fmt.Sprintf("%s#%d", pr.Repo, pr.PRNumber)
		state := pr.PRState
		if state == "" {
			state = "unknown"
		}
		review := pr.ReviewStatus
		if review == "" {
			review = "none"
		}
		taskRef := "-"
		if pr.FsTaskURL != "" {
			taskRef = fmt.Sprintf("[%s](%s)", markdownTableCell(pr.FsTaskID), pr.FsTaskURL)
		} else if pr.FsTaskID != "" {
			taskRef = pr.FsTaskID
		}
		fmt.Fprintf(&b, "| [%s](%s) | %s | %s | %s | %s |\n",
			markdownTableCell(label), pr.PRURL,
			markdownTableCell(escapeMarkdown(truncateText(pr.PRTitle, 200))),
			markdownTableCell(state), markdownTableCell(review), markdownTableCell(taskRef))
	}
	return b.String()
}

// formatTaskRefreshMarkdown renders the outcome of refreshing one task as
// markdown for a card: the refreshed issue state, the per-PR progress, and any
// close suggestion.
func formatTaskRefreshMarkdown(task store.Task, prs []store.TaskPR, suggestion string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Status**: `%s`\n", task.Status)
	if task.IssueURL != "" {
		label := issueRef(task)
		if label == "-" {
			label = task.IssueURL
		}
		fmt.Fprintf(&b, "**Issue**: [%s](%s)", escapeMarkdown(label), task.IssueURL)
		if task.IssueState != "" {
			fmt.Fprintf(&b, " (%s)", task.IssueState)
		}
		b.WriteString("\n")
	}
	b.WriteString(formatTaskFreshness(task))
	if len(prs) > 0 {
		b.WriteString("\n**PRs**:\n")
		b.WriteString(formatTaskPRTable(prs))
	} else {
		b.WriteString("\nNo linked PRs.\n")
	}
	if suggestion != "" {
		fmt.Fprintf(&b, "\n**Suggestion**: %s\n", escapeMarkdown(suggestion))
	}
	return strings.TrimSpace(b.String())
}

// taskRefreshReply refreshes one task (or all non-archived tasks) and reports
// the resulting snapshots.
func (s *server) taskRefreshReply(target string) botReply {
	if target == "all" {
		refreshed, notes, err := s.tasks.RefreshAll()
		if err != nil {
			return botReply{text: "Refresh failed: " + err.Error()}
		}
		return botReply{text: s.tasks.FormatRefreshAll(refreshed, notes)}
	}
	task, prs, suggestion, err := s.tasks.Refresh(target)
	if err != nil {
		return botReply{text: "Refresh failed: " + err.Error()}
	}
	return botReply{
		text: s.tasks.FormatRefreshResult(task, prs, suggestion),
		md: &lark.MarkdownCard{
			Title:   "Refreshed " + task.ID,
			Content: formatTaskRefreshMarkdown(task, prs, suggestion),
		},
	}
}

// reviewStatusCell renders a review's status as a colored tag, appending an
// OUTDATED marker (keeping the status color) when the last reviewed head is
// behind the PR's current head.
func reviewStatusCell(req store.ReviewRequest) any {
	if review.ReviewOutdated(req) {
		return optionTag(req.Status+" / OUTDATED", statusColor(req.Status))
	}
	return statusOption(req.Status)
}

// prsReply lists the open pull requests authored by the current gh user in the
// whitelisted review repos. Each PR's base branch is shown so the merge target
// is distinguishable at a glance. Repos whose gh lookup fails are reported
// instead of hiding the results of the others.
func (s *server) prsReply() botReply {
	repos := s.reviews.RepoList()
	if len(repos) == 0 {
		return botReply{text: "No repos whitelisted. Set SATURNUS_REVIEW_REPOS_FILE to a JSON mapping file, or SATURNUS_REVIEW_REPOS=owner/repo=/path/to/checkout,..."}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	prs, errs := review.ListOpenPRs(ctx, s.reviews.GHCli(), repos)
	if len(prs) == 0 {
		if len(errs) > 0 {
			var b strings.Builder
			b.WriteString("Failed to list open PRs (is gh configured?):\n")
			keys := make([]string, 0, len(errs))
			for repo := range errs {
				keys = append(keys, repo)
			}
			sort.Strings(keys)
			for _, repo := range keys {
				fmt.Fprintf(&b, "%s: %v\n", repo, errs[repo])
			}
			return botReply{text: strings.TrimSpace(b.String())}
		}
		return botReply{text: "No open PRs authored by the current gh user in the whitelisted repos."}
	}
	if len(prs) > 10 {
		prs = prs[:10]
	}
	sort.Slice(prs, func(i, j int) bool {
		if prs[i].Repo != prs[j].Repo {
			return prs[i].Repo < prs[j].Repo
		}
		return prs[i].Number < prs[j].Number
	})
	var b strings.Builder
	rows := make([]map[string]any, 0, len(prs))
	for _, pr := range prs {
		fmt.Fprintf(&b, "%s#%d %s base=%s head=%s updated=%s\n", pr.Repo, pr.Number, pr.Title, pr.BaseBranch, pr.HeadBranch, pr.UpdatedAt)
		rows = append(rows, map[string]any{
			"repo":      fmt.Sprintf("%s#%d", pr.Repo, pr.Number),
			"title":     pr.Title,
			"base":      pr.BaseBranch,
			"head":      pr.HeadBranch,
			"draft":     boolOption(pr.IsDraft),
			"updatedAt": lark.UnixMillis(parseRFC3339(pr.UpdatedAt)),
		})
	}
	return botReply{
		text: strings.TrimSpace(b.String()),
		table: &lark.TableCard{
			Title: "Open PRs",
			Columns: []lark.TableColumn{
				{Name: "repo", Display: "Repo#PR", DataType: lark.ColumnText, Width: "18%"},
				{Name: "title", Display: "Title", DataType: lark.ColumnText, Width: "32%"},
				{Name: "base", Display: "Base", DataType: lark.ColumnText, Width: "12%"},
				{Name: "head", Display: "Head", DataType: lark.ColumnText, Width: "12%"},
				{Name: "draft", Display: "Draft", DataType: lark.ColumnOption, Width: "10%"},
				{Name: "updatedAt", Display: "Updated", DataType: lark.ColumnDate, DateFormat: "YYYY-MM-DD HH:mm", Width: "16%"},
			},
			Rows: rows,
		},
	}
}

// parseRFC3339 parses a gh timestamp, returning the zero time when unparseable.
func parseRFC3339(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return t
}

// filterStatus keeps only reviews whose status equals want.
func filterStatus(reqs []store.ReviewRequest, want string) []store.ReviewRequest {
	out := make([]store.ReviewRequest, 0, len(reqs))
	for _, req := range reqs {
		if req.Status == want {
			out = append(out, req)
		}
	}
	return out
}

// requesterDisplayName returns the requester's display name, falling back to
// the open_id when no name could be resolved or stored.
func requesterDisplayName(req store.ReviewRequest) string {
	if req.RequesterName != "" {
		return req.RequesterName
	}
	return req.RequesterOpenID
}

// groupsReply lists every group chat the bot has joined with its behavior
// mode (default review unless explicitly set).
func (s *server) groupsReply() botReply {
	chats, err := s.lark.ListChats()
	if err != nil {
		return botReply{text: "Failed to list groups: " + err.Error()}
	}
	modes := make(map[string]string)
	for _, m := range s.store.ListChatModes() {
		modes[m.ChatID] = m.Mode
	}
	groups := make([]lark.Chat, 0, len(chats))
	for _, ch := range chats {
		if ch.IsGroupChat() {
			groups = append(groups, ch)
		}
	}
	if len(groups) == 0 {
		return botReply{text: "Bot is not a member of any group chat."}
	}
	sort.Slice(groups, func(i, j int) bool {
		return strings.ToLower(groups[i].Name) < strings.ToLower(groups[j].Name)
	})
	var b strings.Builder
	rows := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		mode := modes[g.ChatID]
		if mode == "" {
			mode = store.ChatModeReview
		}
		name := g.Name
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(&b, "%s  mode=%s\n%s\n", name, mode, g.ChatID)
		rows = append(rows, map[string]any{
			"name":   name,
			"mode":   mode,
			"chatID": g.ChatID,
		})
	}
	return botReply{
		text: strings.TrimSuffix(b.String(), "\n"),
		table: &lark.TableCard{
			Title: "Joined Groups",
			Columns: []lark.TableColumn{
				{Name: "name", Display: "Group", DataType: lark.ColumnText, Width: "38%"},
				{Name: "mode", Display: "Mode", DataType: lark.ColumnText, Width: "14%"},
				{Name: "chatID", Display: "Chat ID", DataType: lark.ColumnText, Width: "48%"},
			},
			Rows: rows,
		},
	}
}

// groupReply handles /sat group and /sat group mode. Without an explicit
// chat_id the current group (msg.ChatID) is used when the command is issued
// inside a group; otherwise the chat_id must be given explicitly.
func (s *server) groupReply(fields []string, text string, msg review.MessageContext) botReply {
	if len(fields) >= 3 && fields[2] == "mode" {
		return s.groupModeReply(fields, msg)
	}
	chatID := ""
	if len(fields) >= 3 {
		chatID = fields[2]
	} else if msg.ChatType == "group" && msg.ChatID != "" {
		chatID = msg.ChatID
	}
	if chatID == "" {
		return botReply{text: "Usage: /sat group [chat_id] or /sat group mode <flags> [chat_id] (flags: review|report, e.g. report|review)"}
	}
	name := chatID
	if s.lark.Enabled() {
		ch, err := s.lark.GetChat(chatID)
		if err != nil {
			if msg.ChatID != chatID {
				return botReply{text: "Cannot find chat " + chatID + ": " + err.Error()}
			}
		} else {
			if ch.Name != "" {
				name = ch.Name
			}
			if ch.IsKnownNonGroup() {
				return botReply{text: fmt.Sprintf("%s is not a group chat (chat_mode=%s).", chatID, ch.ChatMode)}
			}
		}
	}
	return botReply{text: fmt.Sprintf("%s (%s) mode=%s", name, chatID, s.store.GetChatMode(chatID))}
}

// groupModeReply handles /sat group mode <flags> [chat_id]. The mode is a
// `|`-joined flag set (e.g. review, report, report|review). A chat_id passed
// explicitly (outside the group) is validated against the bot's memberships so
// typos fail fast.
func (s *server) groupModeReply(fields []string, msg review.MessageContext) botReply {
	if len(fields) < 4 {
		return botReply{text: "Usage: /sat group mode <review|report|report|review> [chat_id]"}
	}
	mode, err := store.NormalizeChatMode(fields[3])
	if err != nil {
		return botReply{text: "Invalid mode: " + err.Error()}
	}
	chatID := ""
	if len(fields) >= 5 {
		chatID = fields[4]
	} else if msg.ChatType == "group" && msg.ChatID != "" {
		chatID = msg.ChatID
	}
	if chatID == "" {
		return botReply{text: "Not running in a group; provide the group chat_id: /sat group mode <review|report|report|review> <chat_id>"}
	}
	if msg.ChatID != chatID && s.lark.Enabled() {
		ch, err := s.lark.GetChat(chatID)
		if err != nil {
			return botReply{text: "Cannot find chat " + chatID + ": " + err.Error()}
		}
		if ch.IsKnownNonGroup() {
			return botReply{text: chatID + " is not a group chat (chat_mode=" + ch.ChatMode + ")."}
		}
	}
	stored, err := s.store.SetChatMode(chatID, mode)
	if err != nil {
		return botReply{text: "Failed to set group mode: " + err.Error()}
	}
	name := chatID
	if ch, err := s.lark.GetChat(chatID); err == nil && ch.Name != "" {
		name = ch.Name
	}
	return botReply{text: fmt.Sprintf("Group %s (%s) mode is now %s", name, stored.ChatID, stored.Mode)}
}

// refreshReply refreshes one review (or all non-archived reviews) against its
// PR and task, and reports what changed.
func (s *server) refreshReply(target string, msg review.MessageContext) botReply {
	if target == "all" {
		reqs, results, err := s.reviews.RefreshAll(msg)
		if err != nil {
			return botReply{text: "Refresh failed: " + err.Error()}
		}
		return botReply{text: s.reviews.FormatRefreshAll(reqs, results)}
	}
	req, res, err := s.reviews.Refresh(target, msg)
	if err != nil {
		return botReply{text: "Refresh failed: " + err.Error()}
	}
	return botReply{text: s.reviews.FormatRefreshResult(req, res)}
}

// threadsReply renders every chat thread a review was requested in. Each row is
// one thread (chat, topic, participant count, last activity); participants are
// shown as open_ids since resolving every one via the contact API would be too
// slow for the reply budget.
func (s *server) threadsReply(reviewID string) botReply {
	req, err := s.reviews.Get(reviewID)
	if err != nil {
		return botReply{text: "Review not found."}
	}
	threads := s.reviews.ListThreads(reviewID)
	if len(threads) == 0 {
		return botReply{text: "No threads recorded for " + reviewID + "."}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Threads for %s (%s#%d):\n", reviewID, req.Repo, req.PRNumber)
	rows := make([]map[string]any, 0, len(threads))
	for _, t := range threads {
		topic := t.Topic
		if topic == "" {
			topic = "-"
		}
		fmt.Fprintf(&b, "%s chat=%s topic=%s participants=%d last=%s\n",
			t.ID, t.ChatID, topic, len(t.Participants), t.LastMessageAt.Format(time.RFC3339))
		rows = append(rows, map[string]any{
			"id":           t.ID,
			"chat":         t.ChatID,
			"topic":        topic,
			"participants": len(t.Participants),
			"lastActive":   lark.UnixMillis(t.LastMessageAt),
		})
	}
	return botReply{
		text: strings.TrimSpace(b.String()),
		table: &lark.TableCard{
			Title: "Threads for " + reviewID,
			Columns: []lark.TableColumn{
				{Name: "id", Display: "Thread", DataType: lark.ColumnText, Width: "22%"},
				{Name: "chat", Display: "Chat", DataType: lark.ColumnText, Width: "22%"},
				{Name: "topic", Display: "Topic", DataType: lark.ColumnText, Width: "28%"},
				{Name: "participants", Display: "Participants", DataType: lark.ColumnNumber, Width: "13%"},
				{Name: "lastActive", Display: "LastActive", DataType: lark.ColumnDate, DateFormat: "YYYY-MM-DD HH:mm", Width: "15%"},
			},
			Rows: rows,
		},
	}
}

func (s *server) describeReviewReply(id string) botReply {
	req, err := s.reviews.Get(id)
	if err != nil {
		return botReply{text: "Review not found."}
	}
	if err := s.reviews.ResolveRequesterName(&req); err != nil {
		logRequesterResolveFailure(req, err)
	}
	content := formatReviewMarkdown(req, s.reviews.Tool(), s.reviews.ListFindings(req.ID), s.reviews.WorkerDescription(req))
	if contextID, err := s.reviews.ContextID(req.ID); err == nil && contextID != "" {
		content += "\n\n**Context**: " + escapeMarkdown(contextID)
	}
	if runs := s.reviews.ListRuns(req.ID); len(runs) > 0 {
		content += "\n\n" + formatReviewRuns(runs)
	}
	if reacs := s.reviews.ListReactions(req.ID); len(reacs) > 0 {
		content += "\n\n" + formatReviewReactions(reacs)
	}
	if threads := s.reviews.ListThreads(req.ID); len(threads) > 0 {
		content += "\n\n" + formatReviewThreads(threads)
	}
	return botReply{
		text: s.reviews.FormatOne(req),
		md: &lark.MarkdownCard{
			Title:   "Review " + req.ID,
			Content: content,
		},
	}
}

// formatReviewRuns renders a review's run history as a markdown table, so
// every execution (initial + retries, each with its head commit) is visible.
func formatReviewRuns(runs []store.ReviewRun) string {
	var b strings.Builder
	b.WriteString("**Runs**:\n")
	b.WriteString("| # | Status | Worker | Head | Retry reason | Started |\n")
	b.WriteString("| - | ------ | ------ | ---- | ------------ | ------- |\n")
	for _, run := range runs {
		head := run.HeadCommit
		if len(head) > 7 {
			head = head[:7]
		}
		started := ""
		if !run.StartedAt.IsZero() {
			started = run.StartedAt.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(&b, "| %d | %s | %s | `%s` | %s | %s |\n", run.Seq, run.Status, markdownTableCell(review.WorkerLabel(run.Status, run.WorkerID)), head, markdownTableCell(run.RetryReason), started)
	}
	return b.String()
}

// formatReviewReactions renders a review's reaction history as a markdown
// table, so every reviewer-triggered action on the review is visible.
func formatReviewReactions(reacs []store.ReviewReaction) string {
	var b strings.Builder
	b.WriteString("**Reactions**:\n")
	b.WriteString("| # | Status | Worker | Instruction | Updated |\n")
	b.WriteString("| - | ------ | ------ | ----------- | ------- |\n")
	for i, reac := range reacs {
		updated := ""
		if !reac.UpdatedAt.IsZero() {
			updated = reac.UpdatedAt.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n", i+1, reac.Status, markdownTableCell(review.WorkerLabel(reac.Status, reac.WorkerID)), markdownTableCell(truncateText(reac.Instruction, 60)), updated)
	}
	return b.String()
}

// formatReviewThreads renders a review's chat-thread history as a markdown
// table, showing every thread the review was requested in.
func formatReviewThreads(threads []store.ReviewThread) string {
	var b strings.Builder
	b.WriteString("**Threads**:\n")
	b.WriteString("| # | Thread | Chat | Topic | Participants | Last active |\n")
	b.WriteString("| - | ------ | ---- | ----- | ------------ | ----------- |\n")
	for i, t := range threads {
		last := ""
		if !t.LastMessageAt.IsZero() {
			last = t.LastMessageAt.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(&b, "| %d | `%s` | %s | %s | %d | %s |\n",
			i+1, t.ID, markdownTableCell(t.ChatID), markdownTableCell(t.Topic), len(t.Participants), last)
	}
	return b.String()
}

// markdownTableCell sanitizes a value so it does not break a markdown table.
func markdownTableCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "\n", " ")
}

// formatReviewMarkdown renders a single review as markdown for a card. Metadata
// values (title, requester, error) are escaped so they cannot break the
// markdown. The tool output is intentionally left raw since it is itself
// markdown-ish:
//   - for a running review the tail of the output is shown so the latest
//     progress is visible without scrolling through the whole transcript;
//   - for a completed review the full REVIEW SUMMARY block is shown instead
//     (falling back to the output tail when the tool omitted the marker).
func formatReviewMarkdown(req store.ReviewRequest, tool string, findings []store.ReviewFinding, worker string) string {
	var b strings.Builder
	status := req.Status
	if review.ReviewOutdated(req) {
		status += " (OUTDATED)"
	}
	fmt.Fprintf(&b, "**Status**: `%s`\n", status)
	fmt.Fprintf(&b, "**Worker**: %s\n", escapeMarkdown(worker))
	if req.PRURL != "" {
		label := fmt.Sprintf("%s#%d", req.Repo, req.PRNumber)
		fmt.Fprintf(&b, "**PR**: [%s](%s)\n", label, req.PRURL)
	}
	if req.Title != "" {
		fmt.Fprintf(&b, "**Title**: %s\n", escapeMarkdown(req.Title))
	}
	if req.HeadCommit != "" {
		fmt.Fprintf(&b, "**Head**: `%s`\n", req.HeadCommit)
	}
	if review.ReviewOutdated(req) {
		fmt.Fprintf(&b, "**PR head**: `%s` (outdated)\n", req.PrHeadCommit)
	}
	if req.RetryReason != "" {
		fmt.Fprintf(&b, "**Retry reason**: %s\n", escapeMarkdown(req.RetryReason))
	}
	if req.RequesterName != "" || req.RequesterOpenID != "" {
		name := req.RequesterName
		if name == "" {
			name = req.RequesterOpenID
		}
		fmt.Fprintf(&b, "**Requester**: %s\n", escapeMarkdown(name))
	}
	if !req.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "**Created**: %s\n", req.CreatedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if !req.CompletedAt.IsZero() {
		fmt.Fprintf(&b, "**Completed**: %s\n", req.CompletedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if !req.ArchivedAt.IsZero() {
		fmt.Fprintf(&b, "**Archived**: %s\n", req.ArchivedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if !req.RefreshedAt.IsZero() {
		fmt.Fprintf(&b, "**Refreshed**: %s\n", req.RefreshedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if req.TaskID != "" {
		if req.TaskURL != "" {
			fmt.Fprintf(&b, "**Task**: [%s](%s)\n", req.TaskID, req.TaskURL)
		} else {
			fmt.Fprintf(&b, "**Task**: `%s`\n", req.TaskID)
		}
	}
	if !req.CommentsPostedAt.IsZero() {
		fmt.Fprintf(&b, "**Comments posted**: %s\n", req.CommentsPostedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if req.SessionID != "" {
		fmt.Fprintf(&b, "**Session**: `%s` (continue: `%s run --session %s`)\n", req.SessionID, tool, req.SessionID)
	}
	if req.Error != "" {
		fmt.Fprintf(&b, "**Error**: %s\n", escapeMarkdown(req.Error))
	}
	if req.Verdict != "" {
		fmt.Fprintf(&b, "**Verdict**: %s\n", reviewVerdictTag(req.Verdict))
		if req.Summary != "" {
			fmt.Fprintf(&b, "**Summary**: %s\n", escapeMarkdown(req.Summary))
		}
		if len(findings) > 0 {
			b.WriteString("\n**Findings**:\n")
			for _, f := range findings {
				loc := f.Path
				if f.Line > 0 {
					loc = fmt.Sprintf("%s:%d", loc, f.Line)
				}
				if loc == "" {
					loc = "-"
				}
				title := f.Title
				if title == "" {
					title = f.Message
				}
				fmt.Fprintf(&b, "- `%s` **%s** (%s)\n", f.Severity, escapeMarkdown(truncateText(title, 200)), escapeMarkdown(loc))
				if f.Message != "" && f.Message != title {
					fmt.Fprintf(&b, "  %s\n", escapeMarkdown(truncateText(f.Message, 600)))
				}
				if f.Suggestion != "" {
					fmt.Fprintf(&b, "  Fix: %s\n", escapeMarkdown(truncateText(f.Suggestion, 300)))
				}
			}
		}
	}
	if req.ResultText != "" {
		b.WriteString("\n---\n\n")
		if reviewCompleted(req) {
			if summary := review.ExtractSummary(req.ResultText); summary != "" {
				b.WriteString(truncateText(summary, maxSummaryChars))
			} else {
				b.WriteString(tailText(req.ResultText, maxTailChars))
			}
		} else {
			b.WriteString(tailText(req.ResultText, maxTailChars))
		}
	}
	return b.String()
}

// reviewCompleted reports whether a review reached a terminal state.
func reviewCompleted(req store.ReviewRequest) bool {
	return req.Status == "succeeded" || req.Status == "failed" || !req.CompletedAt.IsZero()
}

// maxSummaryChars / maxTailChars bound how much of the tool output a describe
// card shows so it stays within the card content limits.
const (
	maxSummaryChars = 8000
	maxTailChars    = 2000
)

// tailText returns the last max bytes of value, prefixed with a marker when
// earlier output was omitted.
func tailText(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return "...[earlier output omitted]\n" + value[len(value)-max:]
}

func escapeMarkdown(s string) string {
	return strings.NewReplacer(
		"\\", "\\\\",
		"`", "\\`",
		"*", "\\*",
		"_", "\\_",
		"[", "\\[",
		"]", "\\]",
	).Replace(s)
}

func truncateText(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "\n...[truncated]"
}

// satCommandList is the single source of truth for /sat commands, used by both
// /sat help and the plain "/sat" reply. Keep it in sync with the "case" arms of
// handleBotCommand below and with the README.
var satCommandList = []struct {
	command     string
	description string
}{
	{"/sat help", "show this help"},
	{"/sat sessions", "list recent sessions"},
	{"/sat pending", "list pending approvals"},
	{"/sat session <session_id>", "show one session"},
	{"/sat approve <request_id>", "approve a pending request"},
	{"/sat deny <request_id> <reason>", "deny a pending request"},
	{"/sat autopass on|off <session_id>", "toggle auto-approve for a session"},
	{"/sat reviews", "list recent PR reviews (archived reviews are hidden)"},
	{"/sat reviews all|archived", "list PR reviews including archived, or only archived"},
	{"/sat review <pr-url|review_id> [@user]", "start a review (optionally attribute the requester to @user), or show one review by id"},
	{"/sat retry <review_id>", "retry a failed review, or re-run after the PR head changed"},
	{"/sat reset <review_id>", "reset the tool session and queue a fresh review context, even when the PR head is unchanged"},
	{"/sat post <review_id>", "publish the review's findings as GitHub inline comments (plus the verdict review); for reviews whose findings were never posted"},
	{"/sat refresh <review_id|all>", "sync PR/task state: archive merged/closed PRs, complete their task, append reviewer comments to the task"},
	{"/sat act <review_id> <instruction>", "resume the review session to act on the PR (e.g. inline comments for selected issues, concrete code for an issue)"},
	{"/sat threads <review_id>", "list all chat threads a review was requested in"},
	{"/sat describe review <review_id>", "show full details of one review"},
	{"/sat repos", "show the configured review repos"},
	{"/sat prs", "list open PRs (authored by the current gh user) in the whitelisted repos, with base branch"},
	{"/sat groups", "list groups the bot has joined and their modes"},
	{"/sat group [chat_id]", "show a group's mode (defaults to the current group)"},
	{"/sat group mode <flags> [chat_id]", "set a group's mode flags (review, report, or report|review); omit chat_id when running inside the group"},
	{"/sat whoami", "show your open_id"},
	{"/sat whois <name|email|mobile>", "resolve a user to open_id"},
	{"/sat reply <review_id> <message>", "reviewer replies to the requester"},
	{"/sat requester <review_id> @user|ou_xxx", "update a review's requester (mention the user, or pass an open_id)"},
	{"/sat tasks", "list feature tasks (archived tasks are hidden)"},
	{"/sat tasks all|archived", "list tasks including archived, or only archived"},
	{"/sat task <task_id>", "show one task: issue, docs, and per-PR progress"},
	{"/sat task create <issue-url>", "create a task from a GitHub issue"},
	{"/sat task add-doc <task_id> <owner/repo:path>", "attach a local design doc to a task"},
	{"/sat task rm-doc <task_id> <doc_id>", "detach a design doc"},
	{"/sat task add-pr <task_id> <pr-url>", "attach a PR and snapshot its review/Feishu-task state"},
	{"/sat task rm-pr <task_id> <pr_id>", "detach a PR"},
	{"/sat task refresh <task_id|all>", "re-sync issue/PR/review/Feishu-task state"},
	{"/sat task close <task_id>", "archive a task"},
}

func helpText() string {
	var b strings.Builder
	for i, c := range satCommandList {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s - %s", c.command, c.description)
	}
	return b.String()
}

func helpMarkdown() string {
	var b strings.Builder
	for i, c := range satCommandList {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "**%s** — %s", c.command, c.description)
	}
	return b.String()
}

func (s *server) handleBotCommand(text string, msg review.MessageContext) botReply {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return botReply{
			text: helpText(),
			md:   &lark.MarkdownCard{Title: "Saturnus Commands", Content: helpMarkdown()},
		}
	}
	switch fields[1] {
	case "help":
		return botReply{
			text: helpText(),
			md:   &lark.MarkdownCard{Title: "Saturnus Commands", Content: helpMarkdown()},
		}
	case "sessions":
		return s.sessionsReply()
	case "pending":
		return s.pendingReply()
	case "session":
		if len(fields) < 3 {
			return botReply{text: "Usage: /sat session <session_id>"}
		}
		session, events, err := s.store.GetSession(fields[2])
		if err != nil {
			return botReply{text: "Session not found."}
		}
		return botReply{text: fmt.Sprintf("%s status=%s auto=%v events=%d cwd=%s", session.ID, session.Status, session.AutoPass, len(events), session.CWD)}
	case "approve":
		if len(fields) < 3 {
			return botReply{text: "Usage: /sat approve <request_id>"}
		}
		_, _, err := s.store.DecideApproval(fields[2], store.ApprovalDecision{
			Decision:  "approved",
			DecidedBy: "lark",
			Source:    "lark",
		})
		if err != nil {
			return botReply{text: "Approve failed: " + err.Error()}
		}
		return botReply{text: "Approved " + fields[2]}
	case "deny":
		if len(fields) < 3 {
			return botReply{text: "Usage: /sat deny <request_id> <reason>"}
		}
		reason := strings.TrimSpace(strings.TrimPrefix(text, strings.Join(fields[:3], " ")))
		_, _, err := s.store.DecideApproval(fields[2], store.ApprovalDecision{
			Decision:  "denied",
			DecidedBy: "lark",
			Source:    "lark",
			Reason:    reason,
		})
		if err != nil {
			return botReply{text: "Deny failed: " + err.Error()}
		}
		return botReply{text: "Denied " + fields[2]}
	case "autopass":
		if len(fields) < 4 {
			return botReply{text: "Usage: /sat autopass on|off <session_id>"}
		}
		enabled := fields[2] == "on"
		if fields[2] != "on" && fields[2] != "off" {
			return botReply{text: "Usage: /sat autopass on|off <session_id>"}
		}
		session, err := s.store.UpdateAutoPass(fields[3], enabled, 30*time.Minute, "lark", "lark")
		if err != nil {
			return botReply{text: "Auto-pass update failed: " + err.Error()}
		}
		return botReply{text: fmt.Sprintf("Auto-pass for %s is now %v until %s", session.ID, session.AutoPass, session.AutoPassUntil.Format(time.RFC3339))}
	case "whoami":
		if msg.SenderOpenID == "" {
			return botReply{text: "Unknown sender: no open_id in message context."}
		}
		name := msg.SenderName
		if name == "" && s.lark.Enabled() {
			if resolved, err := s.lark.GetUserName(msg.SenderOpenID); err == nil {
				name = resolved
			}
		}
		if name == "" {
			name = "unknown"
		}
		return botReply{text: fmt.Sprintf("%s open_id=%s", name, msg.SenderOpenID)}
	case "whois":
		if len(fields) < 3 {
			return botReply{text: "Usage: /sat whois <name|email|mobile>"}
		}
		query := strings.TrimSpace(strings.TrimPrefix(text, strings.Join(fields[:2], " ")))
		return s.whoisReply(query)
	case "reviews":
		mode := ""
		if len(fields) >= 3 {
			mode = fields[2]
		}
		if mode != "" && mode != "all" && mode != "archived" {
			return botReply{text: "Usage: /sat reviews [all|archived]"}
		}
		return s.reviewsReply(mode)
	case "tasks":
		mode := ""
		if len(fields) >= 3 {
			mode = fields[2]
		}
		if mode != "" && mode != "all" && mode != "archived" {
			return botReply{text: "Usage: /sat tasks [all|archived]"}
		}
		return s.tasksReply(mode)
	case "task":
		return s.taskCommand(fields, text)
	case "repos":
		return botReply{text: s.reviews.FormatRepos()}
	case "prs":
		return s.prsReply()
	case "groups":
		return s.groupsReply()
	case "group":
		return s.groupReply(fields, text, msg)
	case "review":
		if len(fields) < 3 {
			return botReply{text: "Usage: /sat review <pr-url> [@user] or /sat review <review_id>"}
		}
		if strings.HasPrefix(fields[2], "rvw_") {
			req, err := s.reviews.Get(fields[2])
			if err != nil {
				return botReply{text: "Review not found."}
			}
			s.reviews.ResolveRequesterName(&req)
			return botReply{text: s.reviews.FormatOne(req)}
		}
		// Optional requester attribution: a @user mention (the bot itself is
		// excluded), or a trailing open_id (ou_xxx) for conversations where the
		// target user cannot be @mentioned (e.g. a p2p chat with the bot).
		if requester := s.attributedRequester(msg); requester != "" {
			msg.RequesterOpenID = requester
		} else if len(fields) >= 4 && strings.HasPrefix(fields[3], "ou_") {
			msg.RequesterOpenID = fields[3]
		}
		msg.Text = text
		req, err := s.reviews.Submit(msg)
		if err != nil {
			if errors.Is(err, review.ErrAlreadyPending) {
				return botReply{text: "Review already in progress: " + req.ID + " (" + req.PRURL + ") worker=" + s.reviews.WorkerDescription(req)}
			}
			if errors.Is(err, review.ErrReviewUnchanged) {
				return botReply{text: "Review " + req.ID + " is already up to date: PR head commit unchanged since the last review. Use /sat reset " + req.ID + " to run again with a fresh context."}
			}
			return botReply{text: "Review failed: " + err.Error()}
		}
		return botReply{text: "Review started " + req.ID + " (" + req.PRURL + ")"}
	case "retry":
		if len(fields) < 3 {
			return botReply{text: "Usage: /sat retry <review_id>"}
		}
		req, err := s.reviews.Retry(fields[2], msg)
		if err != nil {
			if errors.Is(err, review.ErrAlreadyPending) {
				return botReply{text: "Review already in progress: " + req.ID + " (" + req.PRURL + ") worker=" + s.reviews.WorkerDescription(req)}
			}
			if errors.Is(err, review.ErrReviewUnchanged) {
				return botReply{text: "Review " + fields[2] + " is already up to date: PR head commit unchanged since the last review."}
			}
			return botReply{text: "Retry failed: " + err.Error()}
		}
		return botReply{text: "Retry started " + req.ID + " (" + req.PRURL + ")"}
	case "reset":
		if len(fields) != 3 {
			return botReply{text: "Usage: /sat reset <review_id>"}
		}
		req, err := s.reviews.ResetContext(fields[2], msg)
		if err != nil {
			return botReply{text: "Reset context failed: " + err.Error()}
		}
		return botReply{text: "Review context reset: " + req.ID + ". A fresh tool session will be created on the next run (old session will not be resumed)."}
	case "post":
		if len(fields) < 3 {
			return botReply{text: "Usage: /sat post <review_id>"}
		}
		posted, err := s.reviews.PostComments(fields[2])
		if err != nil {
			return botReply{text: "Post comments failed: " + err.Error()}
		}
		return botReply{text: fmt.Sprintf("Posted %d inline comment(s) on the PR for review %s (verdict review included).", posted, fields[2])}
	case "act":
		if len(fields) < 4 {
			return botReply{text: "Usage: /sat act <review_id> <instruction>"}
		}
		instruction := strings.TrimSpace(strings.TrimPrefix(text, strings.Join(fields[:3], " ")))
		if instruction == "" {
			return botReply{text: "Usage: /sat act <review_id> <instruction>"}
		}
		reac, err := s.reviews.Act(msg, fields[2], instruction)
		if err != nil {
			return botReply{text: "Act failed: " + err.Error()}
		}
		return botReply{text: fmt.Sprintf("Act started %s on review %s: %s", reac.ID, reac.ReviewID, reac.Instruction)}
	case "refresh":
		if len(fields) < 3 {
			return botReply{text: "Usage: /sat refresh <review_id|all>"}
		}
		return s.refreshReply(fields[2], msg)
	case "threads":
		if len(fields) < 3 {
			return botReply{text: "Usage: /sat threads <review_id>"}
		}
		return s.threadsReply(fields[2])
	case "describe":
		if len(fields) < 4 || fields[2] != "review" {
			return botReply{text: "Usage: /sat describe review <review_id>"}
		}
		return s.describeReviewReply(fields[3])
	case "reply":
		if len(fields) < 4 {
			return botReply{text: "Usage: /sat reply <review_id> <message>"}
		}
		message := strings.TrimSpace(strings.TrimPrefix(text, strings.Join(fields[:3], " ")))
		if message == "" {
			return botReply{text: "Usage: /sat reply <review_id> <message>"}
		}
		if err := s.reviews.ReplyToRequester(fields[2], msg.SenderOpenID, message); err != nil {
			return botReply{text: "Reply failed: " + err.Error()}
		}
		return botReply{text: "Replied to requester for " + fields[2]}
	case "requester":
		return s.requesterReply(fields, msg)
	case "cluster-actions":
		if len(fields) < 3 {
			return botReply{text: "Usage: /sat cluster-actions <instance_id> [hours]"}
		}
		hours := 24
		if len(fields) >= 4 {
			if parsed, err := strconv.Atoi(fields[3]); err == nil && parsed > 0 {
				hours = parsed
			}
		}
		return s.clusterActionsReply(fields[2], hours)
	default:
		return botReply{text: "Unknown command. Type /sat help to see all commands."}
	}
}

func (s *server) notifyLarkApproval(req store.ApprovalRequest) {
	chatID := os.Getenv("LARK_NOTIFY_CHAT_ID")
	if chatID == "" {
		return
	}
	text := fmt.Sprintf("Saturnus approval pending\nid=%s\nsession=%s\ntool=%s\nrisk=%s\nUse /sat approve %s or /sat deny %s <reason>", req.ID, req.SessionID, req.ToolName, req.RiskLevel, req.ID, req.ID)
	_ = s.lark.SendText("chat_id", chatID, text)
}

func extractLarkMessage(payload map[string]any) review.MessageContext {
	event, _ := payload["event"].(map[string]any)
	message, _ := event["message"].(map[string]any)
	sender, _ := event["sender"].(map[string]any)
	senderID, _ := sender["sender_id"].(map[string]any)

	var msg review.MessageContext
	msg.ChatID, _ = message["chat_id"].(string)
	msg.ChatType, _ = message["chat_type"].(string)
	msg.MessageID, _ = message["message_id"].(string)
	msg.ReplyTo, _ = message["reply_to"].(string)
	msg.RootMessageID, _ = message["thread_id"].(string)
	if msg.RootMessageID == "" {
		msg.RootMessageID, _ = message["root_id"].(string)
	}
	if msg.RootMessageID == "" {
		msg.RootMessageID = msg.MessageID
	}
	msg.SenderOpenID, _ = senderID["open_id"].(string)
	msg.SenderName, _ = sender["sender_name"].(string)

	if raw, ok := message["mentions"].([]any); ok {
		for _, item := range raw {
			if mention, ok := item.(map[string]any); ok {
				if id, _ := mention["id"].(string); id != "" {
					msg.Mentions = append(msg.Mentions, id)
					if name, _ := mention["name"].(string); name != "" {
						if msg.MentionNames == nil {
							msg.MentionNames = map[string]string{}
						}
						msg.MentionNames[id] = name
					}
				}
			}
		}
	}

	content, _ := message["content"].(string)
	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err == nil && parsed.Text != "" {
		msg.Text = parsed.Text
	} else {
		msg.Text = content
	}
	return msg
}

func reviewConfig(repos map[string]string) review.Config {
	return review.Config{
		Enabled:               os.Getenv("SATURNUS_REVIEW_ENABLED") == "1",
		AllowedUsers:          splitCSV(os.Getenv("SATURNUS_REVIEW_ALLOWED_USERS")),
		ReviewerOpenID:        os.Getenv("SATURNUS_REVIEW_REVIEWER_OPEN_ID"),
		ReviewerChatID:        os.Getenv("SATURNUS_REVIEW_REVIEWER_CHAT_ID"),
		ReplyInThread:         os.Getenv("SATURNUS_REVIEW_REPLY_IN_THREAD") == "1",
		Repos:                 repos,
		Guides:                review.ParseRepoMap(os.Getenv("SATURNUS_REVIEW_GUIDES")),
		Tool:                  os.Getenv("SATURNUS_REVIEW_TOOL"),
		CommandTemplate:       os.Getenv("SATURNUS_REVIEW_COMMAND_TEMPLATE"),
		MaxConcurrent:         envInt("SATURNUS_REVIEW_MAX_CONCURRENT", 1),
		Timeout:               envDuration("SATURNUS_REVIEW_TIMEOUT", 15*time.Minute),
		CreateTask:            os.Getenv("SATURNUS_REVIEW_CREATE_TASK") == "1",
		TaskDueHours:          envInt("SATURNUS_REVIEW_TASK_DUE_HOURS", 24),
		TaskGroup:             os.Getenv("SATURNUS_REVIEW_TASK_GROUP"),
		TaskGroupID:           os.Getenv("SATURNUS_REVIEW_TASK_GROUP_ID"),
		LogDir:                os.Getenv("SATURNUS_REVIEW_LOG_DIR"),
		ResultAPIURL:          os.Getenv("SATURNUS_REVIEW_RESULT_API_URL"),
		ResultToken:           os.Getenv("SATURNUS_REVIEW_RESULT_TOKEN"),
		PostInlineComments:    os.Getenv("SATURNUS_REVIEW_POST_INLINE_COMMENTS") == "1",
		PostInlineMinSeverity: os.Getenv("SATURNUS_REVIEW_POST_INLINE_MIN_SEVERITY"),
		NotifyRequester:       os.Getenv("SATURNUS_REVIEW_NOTIFY_REQUESTER") == "1",
	}
}

// tasksConfig loads the Tasks module config. The repo whitelist is shared with
// the review module (SATURNUS_REVIEW_REPOS or SATURNUS_REVIEW_REPOS_FILE).
func tasksConfig(repos map[string]string) (tasks.Config, error) {
	interval, err := strictDuration("SATURNUS_TASKS_REFRESH_INTERVAL", 5*time.Minute, true)
	if err != nil {
		return tasks.Config{}, err
	}
	timeout, err := strictDuration("SATURNUS_TASKS_REFRESH_TIMEOUT", time.Minute, false)
	if err != nil {
		return tasks.Config{}, err
	}
	return tasks.Config{
		Enabled:         os.Getenv("SATURNUS_TASKS_ENABLED") == "1",
		GHCli:           os.Getenv("SATURNUS_TASKS_GH_CLI"),
		Repos:           repos,
		RefreshInterval: interval, RefreshTimeout: timeout,
	}, nil
}

func strictDuration(name string, fallback time.Duration, allowZero bool) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 || (!allowZero && duration == 0) {
		return 0, fmt.Errorf("invalid %s: %q", name, value)
	}
	return duration, nil
}

// cloudopsFromEnv loads the cloud-ops client config from
// SATURNUS_CLOUDOPS_CONFIG. When the variable is unset the feature is disabled
// and the bot returns "not configured".
func cloudopsFromEnv() (*cloudops.Client, error) {
	path := os.Getenv("SATURNUS_CLOUDOPS_CONFIG")
	if path == "" {
		return nil, nil
	}
	cfg, err := cloudops.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load cloudops config: %w", err)
	}
	return cloudops.New(cfg), nil
}

// clusterActionsReply queries the cloud-ops action history for an instance and
// renders the result. Intentionally not listed in /sat help.
func (s *server) clusterActionsReply(instanceID string, hours int) botReply {
	if s.cloudops == nil {
		return botReply{text: "Cluster actions not configured (set SATURNUS_CLOUDOPS_CONFIG)."}
	}
	if instanceID == "" {
		return botReply{text: "Usage: /sat cluster-actions <instance_id> [hours]"}
	}
	actions, err := s.cloudops.ListActions(context.Background(), instanceID, hours)
	if err != nil {
		return botReply{text: "Cluster actions failed: " + err.Error()}
	}
	card, text := s.cloudops.Reply(instanceID, hours, actions)
	return botReply{text: text, table: card}
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

var satCommandPattern = regexp.MustCompile(`(^|\s)/sat(\s|$)`)

func satCommand(text string) string {
	text = strings.TrimSpace(text)
	loc := satCommandPattern.FindStringIndex(text)
	if loc == nil {
		return ""
	}
	return strings.TrimSpace(text[loc[0]:])
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte(`{}`)
	}
	if err := json.Unmarshal(body, out); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrAlreadyDecided), errors.Is(err, store.ErrInvalidDecision):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func spaFileServer(staticDir string) http.Handler {
	files := http.FileServer(http.Dir(staticDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			files.ServeHTTP(w, r)
			return
		}
		path := filepath.Join(staticDir, filepath.Clean(r.URL.Path))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(staticDir, "index.html"))
	})
}
