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
	"regexp"
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
	store    *store.Store
	lark     *lark.Client
	reviews  *review.Service
	botUsers []string
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
		store:    st,
		lark:     larkClient,
		reviews:  review.New(st, larkClient, reviewConfig()),
		botUsers: splitCSV(os.Getenv("SATURNUS_COMMAND_ALLOWED_USERS")),
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
	s.reviews.ResolveRequesterName(&req)
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
		if errors.Is(err, review.ErrAlreadyPending) {
			writeJSON(w, http.StatusOK, map[string]any{"review": created, "reused": true})
			return
		}
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
	reply := s.handleBotCommand(command, msg)
	s.sendReply(msg, reply)
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
	req, err := s.reviews.SubmitPublic(msg)
	if err != nil {
		if errors.Is(err, review.ErrAlreadyPending) {
			return "Review already in progress: " + req.ID + " (" + req.PRURL + ")"
		}
		return "Review failed: " + err.Error()
	}
	return "Review started " + req.ID + " (" + req.PRURL + ")"
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

func optionTag(text, color string) map[string]any {
	if color == "" {
		return map[string]any{"text": text}
	}
	return map[string]any{"text": text, "color": color}
}

func statusOption(status string) any {
	color := ""
	switch status {
	case "succeeded":
		color = "green"
	case "failed":
		color = "red"
	case "pending":
		color = "blue"
	case "reviewing":
		color = "orange"
	}
	return optionTag(status, color)
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

func (s *server) reviewsReply() botReply {
	reqs := s.reviews.List("")
	if len(reqs) == 0 {
		return botReply{text: "No reviews."}
	}
	if len(reqs) > 10 {
		reqs = reqs[:10]
	}
	rows := make([]map[string]any, 0, len(reqs))
	for _, req := range reqs {
		requester := req.RequesterName
		if requester == "" {
			requester = req.RequesterOpenID
		}
		rows = append(rows, map[string]any{
			"id":        req.ID,
			"status":    statusOption(req.Status),
			"repo":      fmt.Sprintf("%s#%d", req.Repo, req.PRNumber),
			"requester": requester,
			"createdAt": lark.UnixMillis(req.CreatedAt),
		})
	}
	return botReply{
		text: s.reviews.FormatList(s.reviews.List("")),
		table: &lark.TableCard{
			Title: "PR Reviews",
			Columns: []lark.TableColumn{
				{Name: "id", Display: "ID", DataType: lark.ColumnText, Width: "20%"},
				{Name: "status", Display: "Status", DataType: lark.ColumnOption, Width: "12%"},
				{Name: "repo", Display: "Repo#PR", DataType: lark.ColumnText, Width: "25%"},
				{Name: "requester", Display: "Requester", DataType: lark.ColumnText, Width: "26%"},
				{Name: "createdAt", Display: "Created", DataType: lark.ColumnDate, DateFormat: "YYYY-MM-DD HH:mm", Width: "17%"},
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
	s.reviews.ResolveRequesterName(&req)
	return botReply{
		text: s.reviews.FormatOne(req),
		md: &lark.MarkdownCard{
			Title:   "Review " + req.ID,
			Content: formatReviewMarkdown(req, s.reviews.Tool()),
		},
	}
}

// formatReviewMarkdown renders a single review as markdown for a card. Metadata
// values (title, requester, error) are escaped so they cannot break the
// markdown; the review ResultText is intentionally left raw since it is itself
// the tool's markdown-ish output.
func formatReviewMarkdown(req store.ReviewRequest, tool string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Status**: `%s`\n", req.Status)
	if req.PRURL != "" {
		label := fmt.Sprintf("%s#%d", req.Repo, req.PRNumber)
		fmt.Fprintf(&b, "**PR**: [%s](%s)\n", label, req.PRURL)
	}
	if req.Title != "" {
		fmt.Fprintf(&b, "**Title**: %s\n", escapeMarkdown(req.Title))
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
	if req.TaskID != "" {
		if req.TaskURL != "" {
			fmt.Fprintf(&b, "**Task**: [%s](%s)\n", req.TaskID, req.TaskURL)
		} else {
			fmt.Fprintf(&b, "**Task**: `%s`\n", req.TaskID)
		}
	}
	if req.SessionID != "" {
		fmt.Fprintf(&b, "**Session**: `%s` (continue: `%s run --session %s`)\n", req.SessionID, tool, req.SessionID)
	}
	if req.Error != "" {
		fmt.Fprintf(&b, "**Error**: %s\n", escapeMarkdown(req.Error))
	}
	if req.ResultText != "" {
		b.WriteString("\n---\n\n")
		b.WriteString(truncateText(req.ResultText, 3000))
	}
	return b.String()
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
	{"/sat reviews", "list recent PR reviews"},
	{"/sat review <pr-url|review_id>", "start a review, or show one review by id"},
	{"/sat describe review <review_id>", "show full details of one review"},
	{"/sat repos", "show the configured review repos"},
	{"/sat whoami", "show your open_id"},
	{"/sat whois <name|email|mobile>", "resolve a user to open_id"},
	{"/sat reply <review_id> <message>", "reviewer replies to the requester"},
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
		return s.reviewsReply()
	case "repos":
		return botReply{text: s.reviews.FormatRepos()}
	case "review":
		if len(fields) < 3 {
			return botReply{text: "Usage: /sat review <pr-url> or /sat review <review_id>"}
		}
		if strings.HasPrefix(fields[2], "rvw_") {
			req, err := s.reviews.Get(fields[2])
			if err != nil {
				return botReply{text: "Review not found."}
			}
			s.reviews.ResolveRequesterName(&req)
			return botReply{text: s.reviews.FormatOne(req)}
		}
		msg.Text = text
		req, err := s.reviews.Submit(msg)
		if err != nil {
			if errors.Is(err, review.ErrAlreadyPending) {
				return botReply{text: "Review already in progress: " + req.ID + " (" + req.PRURL + ")"}
			}
			return botReply{text: "Review failed: " + err.Error()}
		}
		return botReply{text: "Review started " + req.ID + " (" + req.PRURL + ")"}
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
