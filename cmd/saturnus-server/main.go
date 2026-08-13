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
	"strings"
	"syscall"
	"time"

	"saturnus/internal/lark"
	"saturnus/internal/policy"
	"saturnus/internal/store"
)

type server struct {
	store *store.Store
	lark  *lark.Client
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
	s := &server{
		store: st,
		lark: lark.New(lark.Config{
			AppID:     os.Getenv("LARK_APP_ID"),
			AppSecret: os.Getenv("LARK_APP_SECRET"),
		}),
	}

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

func (s *server) larkEvents(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	if !decodeJSON(w, r, &payload) {
		return
	}
	if challenge, ok := payload["challenge"].(string); ok {
		writeJSON(w, http.StatusOK, map[string]string{"challenge": challenge})
		return
	}
	text, chatID := extractLarkText(payload)
	command := satCommand(text)
	if command == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	reply := s.handleBotCommand(command)
	if chatID != "" {
		_ = s.lark.SendText("chat_id", chatID, reply)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reply": reply})
}

func (s *server) handleBotCommand(text string) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return "Commands: /sat sessions, /sat pending, /sat session <id>, /sat approve <id>, /sat deny <id> <reason>, /sat autopass on|off <session_id>"
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

func extractLarkText(payload map[string]any) (string, string) {
	event, _ := payload["event"].(map[string]any)
	message, _ := event["message"].(map[string]any)
	chatID, _ := message["chat_id"].(string)
	content, _ := message["content"].(string)
	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err == nil && parsed.Text != "" {
		return parsed.Text, chatID
	}
	return content, chatID
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
