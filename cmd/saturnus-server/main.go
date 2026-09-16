package main

import (
	"context"
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"saturnus/internal/lark"
	"saturnus/internal/policy"
	"saturnus/internal/review"
	"saturnus/internal/store"
)

type server struct {
	store   *store.Store
	lark    *lark.Client
	reviews *review.Service
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
	s := &server{
		store:   st,
		lark:    larkClient,
		reviews: review.New(st, larkClient, reviewConfig()),
	}
	s.reviews.Start()
	defer s.reviews.Stop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/agents/register", s.registerAgent)
	mux.HandleFunc("POST /api/agents/{agent_id}/heartbeat", s.heartbeat)
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
	writeJSON(w, http.StatusOK, map[string]any{"reviews": s.reviews.List(r.URL.Query().Get("status"))})
}

func (s *server) getReview(w http.ResponseWriter, r *http.Request) {
	req, err := s.reviews.Get(r.PathValue("review_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"review": req})
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
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"review": created})
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
	command := satCommand(msg.Text)
	if command == "" {
		if reply := s.maybeReview(msg); reply != "" {
			_ = s.lark.SendText("chat_id", msg.ChatID, reply)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	reply := s.handleBotCommand(command, msg)
	if msg.ChatID != "" {
		_ = s.lark.SendText("chat_id", msg.ChatID, reply)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reply": reply})
}

func (s *server) maybeReview(msg review.MessageContext) string {
	if !s.reviews.Enabled() {
		return ""
	}
	if _, err := review.FindPRRequest(msg.Text); err != nil {
		return ""
	}
	req, err := s.reviews.Submit(msg)
	if err != nil {
		return "Review failed: " + err.Error()
	}
	return "Review started " + req.ID + " (" + req.PRURL + ")"
}

func (s *server) handleBotCommand(text string, msg review.MessageContext) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return "Commands: /sat sessions, /sat pending, /sat session <id>, /sat approve <id>, /sat deny <id> <reason>, /sat autopass on|off <session_id>, /sat review <url|id>, /sat reviews, /sat repos, /sat whoami, /sat whois <name|email|mobile>"
	}
	switch fields[1] {
	case "sessions":
		sessions := s.store.ListSessions()
		if len(sessions) == 0 {
			return "No sessions."
		}
		var b strings.Builder
		for i, session := range sessions {
			if i >= 10 {
				break
			}
			fmt.Fprintf(&b, "%s %s auto=%v updated=%s\n", session.ID, session.Status, session.AutoPass, session.UpdatedAt.Format(time.RFC3339))
		}
		return strings.TrimSpace(b.String())
	case "pending":
		approvals := s.store.ListApprovals("pending")
		if len(approvals) == 0 {
			return "No pending approvals."
		}
		var b strings.Builder
		for i, req := range approvals {
			if i >= 10 {
				break
			}
			fmt.Fprintf(&b, "%s session=%s tool=%s risk=%s\n", req.ID, req.SessionID, req.ToolName, req.RiskLevel)
		}
		return strings.TrimSpace(b.String())
	case "session":
		if len(fields) < 3 {
			return "Usage: /sat session <session_id>"
		}
		session, events, err := s.store.GetSession(fields[2])
		if err != nil {
			return "Session not found."
		}
		return fmt.Sprintf("%s status=%s auto=%v events=%d cwd=%s", session.ID, session.Status, session.AutoPass, len(events), session.CWD)
	case "approve":
		if len(fields) < 3 {
			return "Usage: /sat approve <request_id>"
		}
		_, _, err := s.store.DecideApproval(fields[2], store.ApprovalDecision{
			Decision:  "approved",
			DecidedBy: "lark",
			Source:    "lark",
		})
		if err != nil {
			return "Approve failed: " + err.Error()
		}
		return "Approved " + fields[2]
	case "deny":
		if len(fields) < 3 {
			return "Usage: /sat deny <request_id> <reason>"
		}
		reason := strings.TrimSpace(strings.TrimPrefix(text, strings.Join(fields[:3], " ")))
		_, _, err := s.store.DecideApproval(fields[2], store.ApprovalDecision{
			Decision:  "denied",
			DecidedBy: "lark",
			Source:    "lark",
			Reason:    reason,
		})
		if err != nil {
			return "Deny failed: " + err.Error()
		}
		return "Denied " + fields[2]
	case "autopass":
		if len(fields) < 4 {
			return "Usage: /sat autopass on|off <session_id>"
		}
		enabled := fields[2] == "on"
		if fields[2] != "on" && fields[2] != "off" {
			return "Usage: /sat autopass on|off <session_id>"
		}
		session, err := s.store.UpdateAutoPass(fields[3], enabled, 30*time.Minute, "lark", "lark")
		if err != nil {
			return "Auto-pass update failed: " + err.Error()
		}
		return fmt.Sprintf("Auto-pass for %s is now %v until %s", session.ID, session.AutoPass, session.AutoPassUntil.Format(time.RFC3339))
	case "whoami":
		if msg.SenderOpenID == "" {
			return "Unknown sender: no open_id in message context."
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
		return fmt.Sprintf("%s open_id=%s", name, msg.SenderOpenID)
	case "whois":
		if len(fields) < 3 {
			return "Usage: /sat whois <name|email|mobile>"
		}
		query := strings.TrimSpace(strings.TrimPrefix(text, strings.Join(fields[:2], " ")))
		users, err := s.lark.ResolveUser(query)
		if err != nil {
			return "Whois failed: " + err.Error()
		}
		if len(users) == 0 {
			return "No users found for " + query
		}
		var b strings.Builder
		for _, user := range users {
			fmt.Fprintf(&b, "%s open_id=%s", user.Name, user.OpenID)
			if user.Email != "" {
				fmt.Fprintf(&b, " email=%s", user.Email)
			}
			if user.Mobile != "" {
				fmt.Fprintf(&b, " mobile=%s", user.Mobile)
			}
			b.WriteString("\n")
		}
		return strings.TrimSpace(b.String())
	case "reviews":
		return s.reviews.FormatList(s.reviews.List(""))
	case "repos":
		return s.reviews.FormatRepos()
	case "review":
		if len(fields) < 3 {
			return "Usage: /sat review <pr-url> or /sat review <review_id>"
		}
		if strings.HasPrefix(fields[2], "rvw_") {
			req, err := s.reviews.Get(fields[2])
			if err != nil {
				return "Review not found."
			}
			return s.reviews.FormatOne(req)
		}
		msg.Text = text
		req, err := s.reviews.Submit(msg)
		if err != nil {
			return "Review failed: " + err.Error()
		}
		return "Review started " + req.ID + " (" + req.PRURL + ")"
	default:
		return "Unknown command."
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
	msg.MessageID, _ = message["message_id"].(string)
	msg.RootMessageID, _ = message["thread_id"].(string)
	if msg.RootMessageID == "" {
		msg.RootMessageID, _ = message["root_id"].(string)
	}
	if msg.RootMessageID == "" {
		msg.RootMessageID = msg.MessageID
	}
	msg.SenderOpenID, _ = senderID["open_id"].(string)
	msg.SenderName, _ = sender["sender_name"].(string)

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

func reviewConfig() review.Config {
	return review.Config{
		Enabled:         os.Getenv("SATURNUS_REVIEW_ENABLED") == "1",
		AllowedUsers:    splitCSV(os.Getenv("SATURNUS_REVIEW_ALLOWED_USERS")),
		ReviewerOpenID:  os.Getenv("SATURNUS_REVIEW_REVIEWER_OPEN_ID"),
		ReviewerChatID:  os.Getenv("SATURNUS_REVIEW_REVIEWER_CHAT_ID"),
		ReplyInThread:   os.Getenv("SATURNUS_REVIEW_REPLY_IN_THREAD") == "1",
		Repos:           review.ParseRepoMap(os.Getenv("SATURNUS_REVIEW_REPOS")),
		Guides:          review.ParseRepoMap(os.Getenv("SATURNUS_REVIEW_GUIDES")),
		Tool:            os.Getenv("SATURNUS_REVIEW_TOOL"),
		CommandTemplate: os.Getenv("SATURNUS_REVIEW_COMMAND_TEMPLATE"),
		MaxConcurrent:   envInt("SATURNUS_REVIEW_MAX_CONCURRENT", 1),
		Timeout:         envDuration("SATURNUS_REVIEW_TIMEOUT", 15*time.Minute),
		CreateTask:      os.Getenv("SATURNUS_REVIEW_CREATE_TASK") == "1",
		TaskDueHours:    envInt("SATURNUS_REVIEW_TASK_DUE_HOURS", 24),
		LogDir:          os.Getenv("SATURNUS_REVIEW_LOG_DIR"),
	}
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

func satCommand(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "/sat") {
		return text
	}
	if idx := strings.Index(text, "/sat"); idx >= 0 {
		return strings.TrimSpace(text[idx:])
	}
	return ""
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
