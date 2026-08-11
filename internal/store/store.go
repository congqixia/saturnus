package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Agent struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Host      string    `json:"host"`
	Version   string    `json:"version"`
	LastSeen  time.Time `json:"last_seen"`
	CreatedAt time.Time `json:"created_at"`
}

type Session struct {
	ID              string    `json:"id"`
	AgentID         string    `json:"agent_id"`
	CWD             string    `json:"cwd"`
	Repo            string    `json:"repo"`
	Branch          string    `json:"branch"`
	Model           string    `json:"model"`
	Status          string    `json:"status"`
	AutoPass        bool      `json:"auto_pass"`
	AutoPassUntil   time.Time `json:"auto_pass_until,omitempty"`
	AutoPassScope   string    `json:"auto_pass_scope,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	LastHeartbeatAt time.Time `json:"last_heartbeat_at,omitempty"`
}

type Event struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	TurnID    string          `json:"turn_id,omitempty"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

type ApprovalRequest struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	RiskLevel string          `json:"risk_level"`
	Status    string          `json:"status"`
	Reason    string          `json:"reason,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt time.Time       `json:"expires_at"`
}

type ApprovalDecision struct {
	ID        string    `json:"id"`
	RequestID string    `json:"request_id"`
	Decision  string    `json:"decision"`
	DecidedBy string    `json:"decided_by"`
	Source    string    `json:"source"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type AuditLog struct {
	ID        string          `json:"id"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Target    string          `json:"target"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if path == "" {
		path = ":memory:"
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	dsn := path
	if path != ":memory:" {
		dsn = path + "?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE IF NOT EXISTS agents (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			type TEXT NOT NULL,
			host TEXT NOT NULL,
			version TEXT NOT NULL,
			last_seen TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
			cwd TEXT NOT NULL,
			repo TEXT NOT NULL,
			branch TEXT NOT NULL,
			model TEXT NOT NULL,
			status TEXT NOT NULL,
			auto_pass INTEGER NOT NULL DEFAULT 0,
			auto_pass_until TEXT,
			auto_pass_scope TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			last_heartbeat_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_agent_id ON sessions(agent_id)`,
		`CREATE TABLE IF NOT EXISTS events (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			turn_id TEXT NOT NULL DEFAULT '',
			type TEXT NOT NULL,
			payload TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_session_created ON events(session_id, created_at ASC)`,
		`CREATE TABLE IF NOT EXISTS approvals (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			tool_name TEXT NOT NULL,
			tool_input TEXT NOT NULL,
			risk_level TEXT NOT NULL,
			status TEXT NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_approvals_status_created ON approvals(status, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_approvals_session_id ON approvals(session_id)`,
		`CREATE TABLE IF NOT EXISTS decisions (
			id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL UNIQUE REFERENCES approvals(id) ON DELETE CASCADE,
			decision TEXT NOT NULL,
			decided_by TEXT NOT NULL,
			source TEXT NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id TEXT PRIMARY KEY,
			actor TEXT NOT NULL,
			action TEXT NOT NULL,
			target TEXT NOT NULL,
			payload TEXT,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON audit_logs(created_at DESC)`,
	}
	for _, stmt := range statements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) RegisterAgentSession(agent Agent, session Session) (Agent, Session, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Agent{}, Session{}, err
	}
	defer rollback(tx)

	now := time.Now().UTC()
	if agent.ID == "" {
		agent.ID = "agt_" + newID()
	}
	createdAt, err := getCreatedAt(ctx, tx, "agents", agent.ID)
	if err != nil {
		return Agent{}, Session{}, err
	}
	if createdAt.IsZero() {
		agent.CreatedAt = now
	} else {
		agent.CreatedAt = createdAt
	}
	agent.LastSeen = now
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agents (id, name, type, host, version, last_seen, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			type = excluded.type,
			host = excluded.host,
			version = excluded.version,
			last_seen = excluded.last_seen
	`, agent.ID, agent.Name, agent.Type, agent.Host, agent.Version, timeText(agent.LastSeen), timeText(agent.CreatedAt)); err != nil {
		return Agent{}, Session{}, err
	}

	if session.ID == "" {
		session.ID = "ses_" + newID()
	}
	createdAt, err = getCreatedAt(ctx, tx, "sessions", session.ID)
	if err != nil {
		return Agent{}, Session{}, err
	}
	if createdAt.IsZero() {
		session.CreatedAt = now
	} else {
		session.CreatedAt = createdAt
	}
	if session.Status == "" {
		session.Status = "active"
	}
	if session.AutoPass && session.AutoPassUntil.IsZero() {
		session.AutoPassUntil = now.Add(30 * time.Minute)
	}
	session.AgentID = agent.ID
	session.UpdatedAt = now
	session.LastHeartbeatAt = now
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (
			id, agent_id, cwd, repo, branch, model, status, auto_pass, auto_pass_until,
			auto_pass_scope, created_at, updated_at, last_heartbeat_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			agent_id = excluded.agent_id,
			cwd = excluded.cwd,
			repo = excluded.repo,
			branch = excluded.branch,
			model = excluded.model,
			status = excluded.status,
			auto_pass = excluded.auto_pass,
			auto_pass_until = excluded.auto_pass_until,
			auto_pass_scope = excluded.auto_pass_scope,
			updated_at = excluded.updated_at,
			last_heartbeat_at = excluded.last_heartbeat_at
	`, session.ID, session.AgentID, session.CWD, session.Repo, session.Branch, session.Model, session.Status,
		boolInt(session.AutoPass), nullableTimeText(session.AutoPassUntil), session.AutoPassScope, timeText(session.CreatedAt),
		timeText(session.UpdatedAt), nullableTimeText(session.LastHeartbeatAt)); err != nil {
		return Agent{}, Session{}, err
	}
	if err := audit(ctx, tx, "agent:"+agent.ID, "session.register", session.ID, mustJSON(session)); err != nil {
		return Agent{}, Session{}, err
	}
	return agent, session, tx.Commit()
}

func (s *Store) Heartbeat(agentID string) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)

	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE agents SET last_seen = ? WHERE id = ?`, timeText(now), agentID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET last_heartbeat_at = ?, updated_at = ?
		WHERE agent_id = ? AND status = 'active'
	`, timeText(now), timeText(now), agentID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListSessions() []Session {
	rows, err := s.db.Query(`
		SELECT id, agent_id, cwd, repo, branch, model, status, auto_pass, auto_pass_until,
			auto_pass_scope, created_at, updated_at, last_heartbeat_at
		FROM sessions
		ORDER BY updated_at DESC
	`)
	if err != nil {
		return []Session{}
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err == nil {
			out = append(out, session)
		}
	}
	return out
}

func (s *Store) GetSession(id string) (Session, []Event, error) {
	row := s.db.QueryRow(`
		SELECT id, agent_id, cwd, repo, branch, model, status, auto_pass, auto_pass_until,
			auto_pass_scope, created_at, updated_at, last_heartbeat_at
		FROM sessions
		WHERE id = ?
	`, id)
	session, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, nil, ErrNotFound
	}
	if err != nil {
		return Session{}, nil, err
	}

	rows, err := s.db.Query(`
		SELECT id, session_id, turn_id, type, payload, created_at
		FROM events
		WHERE session_id = ?
		ORDER BY created_at ASC
	`, id)
	if err != nil {
		return Session{}, nil, err
	}
	defer rows.Close()

	events := []Event{}
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return Session{}, nil, err
		}
		events = append(events, event)
	}
	return session, events, rows.Err()
}

func (s *Store) UpdateAutoPass(sessionID string, enabled bool, ttl time.Duration, scope string, actor string) (Session, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer rollback(tx)

	session, err := getSessionTx(ctx, tx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	session.AutoPass = enabled
	session.AutoPassScope = scope
	if enabled {
		session.AutoPassUntil = time.Now().UTC().Add(ttl)
	} else {
		session.AutoPassUntil = time.Time{}
	}
	session.UpdatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET auto_pass = ?, auto_pass_until = ?, auto_pass_scope = ?, updated_at = ?
		WHERE id = ?
	`, boolInt(session.AutoPass), nullableTimeText(session.AutoPassUntil), session.AutoPassScope, timeText(session.UpdatedAt), session.ID); err != nil {
		return Session{}, err
	}
	if err := audit(ctx, tx, actor, "session.autopass", sessionID, mustJSON(session)); err != nil {
		return Session{}, err
	}
	return session, tx.Commit()
}

func (s *Store) AddEvent(event Event) (Event, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, err
	}
	defer rollback(tx)

	session, err := getSessionTx(ctx, tx, event.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, err
	}
	if event.ID == "" {
		event.ID = "evt_" + newID()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	if len(event.Payload) == 0 {
		event.Payload = json.RawMessage(`{}`)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO events (id, session_id, turn_id, type, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, event.ID, event.SessionID, event.TurnID, event.Type, string(event.Payload), timeText(event.CreatedAt)); err != nil {
		return Event{}, err
	}
	session.UpdatedAt = event.CreatedAt
	if event.Type == "stop" {
		session.Status = "stopped"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET status = ?, updated_at = ? WHERE id = ?`, session.Status, timeText(session.UpdatedAt), session.ID); err != nil {
		return Event{}, err
	}
	if err := audit(ctx, tx, "agent:"+session.AgentID, "event.add", event.ID, mustJSON(event)); err != nil {
		return Event{}, err
	}
	return event, tx.Commit()
}

func (s *Store) CreateApproval(req ApprovalRequest) (ApprovalRequest, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ApprovalRequest{}, err
	}
	defer rollback(tx)

	if _, err := getSessionTx(ctx, tx, req.SessionID); errors.Is(err, sql.ErrNoRows) {
		return ApprovalRequest{}, ErrNotFound
	} else if err != nil {
		return ApprovalRequest{}, err
	}
	if req.ID == "" {
		req.ID = "apr_" + newID()
	}
	if req.Status == "" {
		req.Status = "pending"
	}
	if req.CreatedAt.IsZero() {
		req.CreatedAt = time.Now().UTC()
	}
	if req.ExpiresAt.IsZero() {
		req.ExpiresAt = req.CreatedAt.Add(10 * time.Minute)
	}
	if len(req.ToolInput) == 0 {
		req.ToolInput = json.RawMessage(`{}`)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO approvals (id, session_id, tool_name, tool_input, risk_level, status, reason, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, req.ID, req.SessionID, req.ToolName, string(req.ToolInput), req.RiskLevel, req.Status, req.Reason, timeText(req.CreatedAt), timeText(req.ExpiresAt)); err != nil {
		return ApprovalRequest{}, err
	}
	if err := audit(ctx, tx, "agent:"+req.SessionID, "approval.create", req.ID, mustJSON(req)); err != nil {
		return ApprovalRequest{}, err
	}
	return req, tx.Commit()
}

func (s *Store) ListApprovals(status string) []ApprovalRequest {
	query := `
		SELECT id, session_id, tool_name, tool_input, risk_level, status, reason, created_at, expires_at
		FROM approvals
	`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return []ApprovalRequest{}
	}
	defer rows.Close()

	out := []ApprovalRequest{}
	for rows.Next() {
		req, err := scanApproval(rows)
		if err == nil {
			out = append(out, req)
		}
	}
	return out
}

func (s *Store) DecideApproval(requestID string, decision ApprovalDecision) (ApprovalRequest, ApprovalDecision, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ApprovalRequest{}, ApprovalDecision{}, err
	}
	defer rollback(tx)

	req, err := getApprovalTx(ctx, tx, requestID)
	if errors.Is(err, sql.ErrNoRows) {
		return ApprovalRequest{}, ApprovalDecision{}, ErrNotFound
	}
	if err != nil {
		return ApprovalRequest{}, ApprovalDecision{}, err
	}
	if req.Status != "pending" {
		return req, ApprovalDecision{}, ErrAlreadyDecided
	}
	if decision.Decision != "approved" && decision.Decision != "denied" {
		return ApprovalRequest{}, ApprovalDecision{}, ErrInvalidDecision
	}
	if decision.ID == "" {
		decision.ID = "dec_" + newID()
	}
	decision.RequestID = requestID
	if decision.CreatedAt.IsZero() {
		decision.CreatedAt = time.Now().UTC()
	}
	req.Status = decision.Decision
	req.Reason = decision.Reason
	if _, err := tx.ExecContext(ctx, `UPDATE approvals SET status = ?, reason = ? WHERE id = ?`, req.Status, req.Reason, requestID); err != nil {
		return ApprovalRequest{}, ApprovalDecision{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO decisions (id, request_id, decision, decided_by, source, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, decision.ID, decision.RequestID, decision.Decision, decision.DecidedBy, decision.Source, decision.Reason, timeText(decision.CreatedAt)); err != nil {
		return ApprovalRequest{}, ApprovalDecision{}, err
	}
	if err := audit(ctx, tx, decision.DecidedBy, "approval."+decision.Decision, requestID, mustJSON(decision)); err != nil {
		return ApprovalRequest{}, ApprovalDecision{}, err
	}
	return req, decision, tx.Commit()
}

func (s *Store) GetApproval(id string) (ApprovalRequest, bool) {
	req, err := getApproval(context.Background(), s.db, id)
	return req, err == nil
}

type sessionScanner interface {
	Scan(dest ...any) error
}

type eventScanner interface {
	Scan(dest ...any) error
}

type approvalScanner interface {
	Scan(dest ...any) error
}

func getSessionTx(ctx context.Context, tx *sql.Tx, id string) (Session, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT id, agent_id, cwd, repo, branch, model, status, auto_pass, auto_pass_until,
			auto_pass_scope, created_at, updated_at, last_heartbeat_at
		FROM sessions
		WHERE id = ?
	`, id)
	return scanSession(row)
}

func getApproval(ctx context.Context, db *sql.DB, id string) (ApprovalRequest, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, session_id, tool_name, tool_input, risk_level, status, reason, created_at, expires_at
		FROM approvals
		WHERE id = ?
	`, id)
	return scanApproval(row)
}

func getApprovalTx(ctx context.Context, tx *sql.Tx, id string) (ApprovalRequest, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT id, session_id, tool_name, tool_input, risk_level, status, reason, created_at, expires_at
		FROM approvals
		WHERE id = ?
	`, id)
	return scanApproval(row)
}

func getCreatedAt(ctx context.Context, tx *sql.Tx, table string, id string) (time.Time, error) {
	if table != "agents" && table != "sessions" {
		return time.Time{}, fmt.Errorf("invalid table: %s", table)
	}
	var raw sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT created_at FROM `+table+` WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return parseNullTime(raw)
}

func scanSession(row sessionScanner) (Session, error) {
	var session Session
	var autoPass int
	var autoPassUntil sql.NullString
	var lastHeartbeatAt sql.NullString
	var createdAt string
	var updatedAt string
	if err := row.Scan(
		&session.ID, &session.AgentID, &session.CWD, &session.Repo, &session.Branch, &session.Model,
		&session.Status, &autoPass, &autoPassUntil, &session.AutoPassScope, &createdAt, &updatedAt,
		&lastHeartbeatAt,
	); err != nil {
		return Session{}, err
	}
	session.AutoPass = autoPass == 1
	session.AutoPassUntil, _ = parseNullTime(autoPassUntil)
	session.LastHeartbeatAt, _ = parseNullTime(lastHeartbeatAt)
	session.CreatedAt, _ = parseTime(createdAt)
	session.UpdatedAt, _ = parseTime(updatedAt)
	return session, nil
}

func scanEvent(row eventScanner) (Event, error) {
	var event Event
	var payload string
	var createdAt string
	if err := row.Scan(&event.ID, &event.SessionID, &event.TurnID, &event.Type, &payload, &createdAt); err != nil {
		return Event{}, err
	}
	event.Payload = json.RawMessage(payload)
	event.CreatedAt, _ = parseTime(createdAt)
	return event, nil
}

func scanApproval(row approvalScanner) (ApprovalRequest, error) {
	var req ApprovalRequest
	var toolInput string
	var createdAt string
	var expiresAt string
	if err := row.Scan(&req.ID, &req.SessionID, &req.ToolName, &toolInput, &req.RiskLevel, &req.Status, &req.Reason, &createdAt, &expiresAt); err != nil {
		return ApprovalRequest{}, err
	}
	req.ToolInput = json.RawMessage(toolInput)
	req.CreatedAt, _ = parseTime(createdAt)
	req.ExpiresAt, _ = parseTime(expiresAt)
	return req, nil
}

func audit(ctx context.Context, tx *sql.Tx, actor, action, target string, payload json.RawMessage) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO audit_logs (id, actor, action, target, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, "aud_"+newID(), actor, action, target, string(payload), timeText(time.Now().UTC()))
	return err
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullableTimeText(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return timeText(t)
}

func timeText(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, raw)
}

func parseNullTime(raw sql.NullString) (time.Time, error) {
	if !raw.Valid || raw.String == "" {
		return time.Time{}, nil
	}
	return parseTime(raw.String)
}

var (
	ErrNotFound        = errors.New("not found")
	ErrAlreadyDecided  = errors.New("approval already decided")
	ErrInvalidDecision = errors.New("invalid decision")
)

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format("20060102150405.000000000")))
	}
	return hex.EncodeToString(b[:])
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
