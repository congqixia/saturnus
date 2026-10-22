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
	"sort"
	"strings"
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

type ReviewThread struct {
	ID            string    `json:"id"`
	ChatID        string    `json:"chat_id"`
	RootMessageID string    `json:"root_message_id"`
	Topic         string    `json:"topic"`
	Participants  []string  `json:"participants"`
	LastMessageAt time.Time `json:"last_message_at"`
	CreatedAt     time.Time `json:"created_at"`
}

// ReviewRun is one append-only execution of a review (initial run or a
// retry/continue). It keeps the outcome of every run, including the PR head
// commit it reviewed, so history can be traced after later runs overwrite the
// review_requests row.
type ReviewRun struct {
	ID          string    `json:"id"`
	ReviewID    string    `json:"review_id"`
	Seq         int       `json:"seq"`
	Status      string    `json:"status"`
	HeadCommit  string    `json:"head_commit"`
	RetryReason string    `json:"retry_reason"`
	SessionID   string    `json:"session_id"`
	ResultText  string    `json:"result_text"`
	Error       string    `json:"error"`
	Tool        string    `json:"tool"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ReviewRequest struct {
	ID              string    `json:"id"`
	ThreadID        string    `json:"thread_id"`
	Status          string    `json:"status"`
	PRURL           string    `json:"pr_url"`
	Repo            string    `json:"repo"`
	PRNumber        int       `json:"pr_number"`
	Title           string    `json:"title"`
	BaseBranch      string    `json:"base_branch"`
	RequesterOpenID string    `json:"requester_open_id"`
	RequesterName   string    `json:"requester_name"`
	ChatID          string    `json:"chat_id"`
	MessageID       string    `json:"message_id"`
	Tool            string    `json:"tool"`
	TaskID          string    `json:"task_id"`
	TaskURL         string    `json:"task_url"`
	SessionID       string    `json:"session_id"`
	HeadCommit      string    `json:"head_commit"`
	PrHeadCommit    string    `json:"pr_head_commit"`
	RetryReason     string    `json:"retry_reason"`
	ResultText      string    `json:"result_text"`
	Error           string    `json:"error"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	CompletedAt     time.Time `json:"completed_at,omitempty"`
	// ArchivedAt is set when the review is archived because its PR was merged
	// or closed. Archived reviews are hidden from default listings.
	ArchivedAt time.Time `json:"archived_at,omitempty"`
	// RefreshedAt records the last /sat refresh of this review.
	RefreshedAt time.Time `json:"refreshed_at,omitempty"`
	// TaskSyncedAt is the submitted_at of the newest PR review already synced
	// to the Feishu task, so later refreshes only append new reviewer comments.
	TaskSyncedAt time.Time `json:"task_synced_at,omitempty"`
}

// ReviewReaction is one reviewer-triggered action taken on a finished review by
// resuming its tool session (e.g. post inline comments on the PR for selected
// issues, or work out concrete code for an issue). It is append-only history,
// analogous to review_runs but tied to reviewer instructions rather than review
// executions.
type ReviewReaction struct {
	ID           string    `json:"id"`
	ReviewID     string    `json:"review_id"`
	Instruction  string    `json:"instruction"`
	Status       string    `json:"status"`
	ChatID       string    `json:"chat_id"`
	MessageID    string    `json:"message_id"`
	SenderOpenID string    `json:"sender_open_id"`
	SenderName   string    `json:"sender_name"`
	SessionID    string    `json:"session_id"`
	ResultText   string    `json:"result_text"`
	Error        string    `json:"error"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	CompletedAt  time.Time `json:"completed_at,omitempty"`
}

// Task is a feature tracking record that binds one GitHub issue to its local
// design docs and implementing PRs. It is distinct from the Feishu tasks the
// review pipeline creates; Feishu task references on linked PRs are stored as
// fs_task_id/fs_task_url.
type Task struct {
	ID string `json:"id"`
	// Title is the human-readable task title.
	Title string `json:"title"`
	// Status is open / in_progress / done / archived.
	Status string `json:"status"`
	// IssueRepo and IssueNumber identify the bound GitHub issue.
	IssueRepo   string `json:"issue_repo"`
	IssueNumber int    `json:"issue_number"`
	// IssueTitle and IssueState are snapshots from `gh issue view`.
	IssueTitle string `json:"issue_title"`
	IssueState string `json:"issue_state"`
	// IssueURL is the canonical issue URL.
	IssueURL    string `json:"issue_url"`
	Description string `json:"description"`
	// ArchivedAt is set when the task is closed; archived tasks are hidden from
	// default listings.
	ArchivedAt time.Time `json:"archived_at,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// TaskDoc is one attached local design doc: a repo-relative path inside the
// mapped checkout of a whitelisted repo.
type TaskDoc struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	Repo   string `json:"repo"`
	Path   string `json:"path"`
	// Title defaults to the file base name when not set.
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskPR is one linked PR with its per-PR progress snapshot. The review status
// and Feishu task references are copied from the linked review_requests row
// (via ReviewID) on attach and refresh, so the row is self-contained and does
// not depend on live review lookups to render.
type TaskPR struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	Repo   string `json:"repo"`
	// PRNumber and PRURL identify the PR.
	PRNumber int    `json:"pr_number"`
	PRURL    string `json:"pr_url"`
	// PRTitle, PRState and BaseBranch are snapshots from `gh pr view`. PRState
	// is OPEN / CLOSED / MERGED.
	PRTitle    string `json:"pr_title"`
	PRState    string `json:"pr_state"`
	BaseBranch string `json:"base_branch"`
	// ReviewID links to review_requests when a review exists; empty otherwise.
	ReviewID string `json:"review_id"`
	// ReviewStatus is the snapshot of the linked review's status, or "none".
	ReviewStatus string `json:"review_status"`
	// FsTaskID / FsTaskURL snapshot the Feishu task created for the review.
	FsTaskID  string `json:"fs_task_id"`
	FsTaskURL string `json:"fs_task_url"`
	// SyncedAt records the last time the snapshot was refreshed.
	SyncedAt  time.Time `json:"synced_at,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Chat mode flags control how the Lark bot behaves inside a group chat. A
// chat's mode is a `|`-joined set of flags (e.g. "report|review"), so
// capabilities can be combined instead of being mutually exclusive.
const (
	// ChatModeReview is the default: the bot answers direct mentions and can
	// run reviews and /sat commands in the group.
	ChatModeReview = "review"
	// ChatModeReport marks the group as a quiet report channel: the bot does
	// not respond to ordinary messages. When combined with ChatModeReview
	// ("report|review") it still acts on review requests, while other /sat
	// commands and casual chatter are ignored.
	ChatModeReport = "report"
)

// ChatMode stores the per-chat behavior mode for a group chat.
type ChatMode struct {
	ChatID    string    `json:"chat_id"`
	Mode      string    `json:"mode"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NormalizeChatMode parses a user-supplied mode string (flags separated by
// "|", "," or whitespace), validates each flag, and returns the canonical
// form: flags sorted and joined by "|". It errors on unknown flags or an
// empty set.
func NormalizeChatMode(mode string) (string, error) {
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(mode, func(r rune) bool {
		return r == '|' || r == ',' || r == ' ' || r == '\t'
	}) {
		if part == "" {
			continue
		}
		if part != ChatModeReview && part != ChatModeReport {
			return "", fmt.Errorf("unknown mode flag %q (allowed: review, report)", part)
		}
		seen[part] = true
	}
	if len(seen) == 0 {
		return "", errors.New("empty chat mode (need at least one of review, report)")
	}
	flags := make([]string, 0, len(seen))
	for flag := range seen {
		flags = append(flags, flag)
	}
	sort.Strings(flags)
	return strings.Join(flags, "|"), nil
}

// ChatModeFlags splits a canonical mode string into a flag set.
func ChatModeFlags(mode string) map[string]bool {
	flags := map[string]bool{}
	for _, flag := range strings.Split(mode, "|") {
		if flag = strings.TrimSpace(flag); flag != "" {
			flags[flag] = true
		}
	}
	return flags
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
		`CREATE TABLE IF NOT EXISTS review_threads (
			id TEXT PRIMARY KEY,
			chat_id TEXT NOT NULL,
			root_message_id TEXT NOT NULL DEFAULT '',
			topic TEXT NOT NULL DEFAULT '',
			participants TEXT NOT NULL DEFAULT '[]',
			last_message_at TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_review_threads_key ON review_threads(chat_id, root_message_id)`,
		`CREATE TABLE IF NOT EXISTS review_requests (
			id TEXT PRIMARY KEY,
			thread_id TEXT NOT NULL REFERENCES review_threads(id) ON DELETE CASCADE,
			status TEXT NOT NULL,
			pr_url TEXT NOT NULL,
			repo TEXT NOT NULL,
			pr_number INTEGER NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			base_branch TEXT NOT NULL DEFAULT '',
			requester_open_id TEXT NOT NULL DEFAULT '',
			requester_name TEXT NOT NULL DEFAULT '',
			chat_id TEXT NOT NULL DEFAULT '',
			message_id TEXT NOT NULL DEFAULT '',
			tool TEXT NOT NULL DEFAULT '',
			task_id TEXT NOT NULL DEFAULT '',
			task_url TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '',
			head_commit TEXT NOT NULL DEFAULT '',
			retry_reason TEXT NOT NULL DEFAULT '',
			result_text TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			completed_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_review_requests_created ON review_requests(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_review_requests_status ON review_requests(status, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_review_requests_thread ON review_requests(thread_id)`,
		`CREATE TABLE IF NOT EXISTS review_runs (
			id TEXT PRIMARY KEY,
			review_id TEXT NOT NULL REFERENCES review_requests(id) ON DELETE CASCADE,
			seq INTEGER NOT NULL,
			status TEXT NOT NULL,
			head_commit TEXT NOT NULL DEFAULT '',
			retry_reason TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '',
			result_text TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			tool TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			completed_at TEXT,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_review_runs_review ON review_runs(review_id, seq)`,
		`CREATE TABLE IF NOT EXISTS review_reactions (
			id TEXT PRIMARY KEY,
			review_id TEXT NOT NULL REFERENCES review_requests(id) ON DELETE CASCADE,
			instruction TEXT NOT NULL,
			status TEXT NOT NULL,
			chat_id TEXT NOT NULL DEFAULT '',
			message_id TEXT NOT NULL DEFAULT '',
			sender_open_id TEXT NOT NULL DEFAULT '',
			sender_name TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '',
			result_text TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			completed_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_review_reactions_review ON review_reactions(review_id, created_at ASC)`,
		`CREATE TABLE IF NOT EXISTS review_thread_links (
			review_id TEXT NOT NULL REFERENCES review_requests(id) ON DELETE CASCADE,
			thread_id TEXT NOT NULL REFERENCES review_threads(id) ON DELETE CASCADE,
			linked_at TEXT NOT NULL,
			PRIMARY KEY (review_id, thread_id)
		)`,
		`CREATE TABLE IF NOT EXISTS chat_modes (
			chat_id TEXT PRIMARY KEY,
			mode TEXT NOT NULL DEFAULT 'review',
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'open',
			issue_repo TEXT NOT NULL DEFAULT '',
			issue_number INTEGER NOT NULL DEFAULT 0,
			issue_title TEXT NOT NULL DEFAULT '',
			issue_state TEXT NOT NULL DEFAULT '',
			issue_url TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			archived_at TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_created ON tasks(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS task_docs (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			repo TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			UNIQUE(task_id, repo, path)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_task_docs_task ON task_docs(task_id)`,
		`CREATE TABLE IF NOT EXISTS task_prs (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			repo TEXT NOT NULL DEFAULT '',
			pr_number INTEGER NOT NULL,
			pr_url TEXT NOT NULL DEFAULT '',
			pr_title TEXT NOT NULL DEFAULT '',
			pr_state TEXT NOT NULL DEFAULT '',
			base_branch TEXT NOT NULL DEFAULT '',
			review_id TEXT NOT NULL DEFAULT '',
			review_status TEXT NOT NULL DEFAULT 'none',
			fs_task_id TEXT NOT NULL DEFAULT '',
			fs_task_url TEXT NOT NULL DEFAULT '',
			synced_at TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(task_id, repo, pr_number)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_task_prs_task ON task_prs(task_id)`,
	}
	for _, stmt := range statements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if err := s.ensureColumn(ctx, "review_requests", "session_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "review_requests", "task_url", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "review_requests", "head_commit", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "review_requests", "pr_head_commit", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "review_requests", "retry_reason", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "review_requests", "archived_at", "TEXT"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "review_requests", "refreshed_at", "TEXT"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "review_requests", "task_synced_at", "TEXT"); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+definition)
	return err
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

func (s *Store) GetOrCreateReviewThread(chatID, rootMessageID, topic string, participants []string) (ReviewThread, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewThread{}, err
	}
	defer rollback(tx)

	row := tx.QueryRowContext(ctx, `
		SELECT id, chat_id, root_message_id, topic, participants, last_message_at, created_at
		FROM review_threads
		WHERE chat_id = ? AND root_message_id = ?
	`, chatID, rootMessageID)
	thread, err := scanReviewThread(row)
	if err == nil {
		merged := mergeParticipants(thread.Participants, participants)
		thread.Participants = merged
		thread.LastMessageAt = time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `
			UPDATE review_threads
			SET topic = ?, participants = ?, last_message_at = ?
			WHERE id = ?
		`, firstNonEmpty(topic, thread.Topic), mustJSON(merged), timeText(thread.LastMessageAt), thread.ID); err != nil {
			return ReviewThread{}, err
		}
		return thread, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ReviewThread{}, err
	}

	now := time.Now().UTC()
	thread = ReviewThread{
		ID:            "thr_" + newID(),
		ChatID:        chatID,
		RootMessageID: rootMessageID,
		Topic:         topic,
		Participants:  mergeParticipants(nil, participants),
		LastMessageAt: now,
		CreatedAt:     now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO review_threads (id, chat_id, root_message_id, topic, participants, last_message_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, thread.ID, thread.ChatID, thread.RootMessageID, thread.Topic, mustJSON(thread.Participants), timeText(thread.LastMessageAt), timeText(thread.CreatedAt)); err != nil {
		return ReviewThread{}, err
	}
	return thread, tx.Commit()
}

// GetThread loads one review thread by id.
func (s *Store) GetThread(id string) (ReviewThread, error) {
	row := s.db.QueryRow(`
		SELECT id, chat_id, root_message_id, topic, participants, last_message_at, created_at
		FROM review_threads
		WHERE id = ?
	`, id)
	thread, err := scanReviewThread(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewThread{}, ErrNotFound
	}
	if err != nil {
		return ReviewThread{}, err
	}
	return thread, nil
}

// LinkReviewThread records that a review request happened in a thread. The same
// (review_id, thread_id) pair is deduped, bumping linked_at to the most recent
// time the thread was used, so the review's full thread history is preserved
// even after review_requests.ThreadID moves to a newer thread.
func (s *Store) LinkReviewThread(reviewID, threadID string) error {
	if reviewID == "" || threadID == "" {
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO review_thread_links (review_id, thread_id, linked_at)
		VALUES (?, ?, ?)
		ON CONFLICT(review_id, thread_id) DO UPDATE SET linked_at = excluded.linked_at
	`, reviewID, threadID, timeText(time.Now().UTC()))
	return err
}

// ListReviewThreads returns every thread a review has been requested in, most
// recently active first.
func (s *Store) ListReviewThreads(reviewID string) []ReviewThread {
	rows, err := s.db.Query(`
		SELECT t.id, t.chat_id, t.root_message_id, t.topic, t.participants, t.last_message_at, t.created_at
		FROM review_thread_links l
		JOIN review_threads t ON t.id = l.thread_id
		WHERE l.review_id = ?
		ORDER BY t.last_message_at DESC
	`, reviewID)
	if err != nil {
		return []ReviewThread{}
	}
	defer rows.Close()

	out := []ReviewThread{}
	for rows.Next() {
		thread, err := scanReviewThread(rows)
		if err == nil {
			out = append(out, thread)
		}
	}
	return out
}

func (s *Store) CreateReview(req ReviewRequest) (ReviewRequest, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewRequest{}, err
	}
	defer rollback(tx)

	if _, err := getReviewThreadTx(ctx, tx, req.ThreadID); errors.Is(err, sql.ErrNoRows) {
		return ReviewRequest{}, ErrNotFound
	} else if err != nil {
		return ReviewRequest{}, err
	}
	if req.ID == "" {
		req.ID = "rvw_" + newID()
	}
	if req.Status == "" {
		req.Status = "pending"
	}
	if req.CreatedAt.IsZero() {
		req.CreatedAt = time.Now().UTC()
	}
	req.UpdatedAt = req.CreatedAt
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO review_requests (
			id, thread_id, status, pr_url, repo, pr_number, title, base_branch,
			requester_open_id, requester_name, chat_id, message_id, tool, task_id, task_url,
			session_id, head_commit, pr_head_commit, retry_reason, result_text, error, created_at, updated_at, completed_at,
			archived_at, refreshed_at, task_synced_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, req.ID, req.ThreadID, req.Status, req.PRURL, req.Repo, req.PRNumber, req.Title, req.BaseBranch,
		req.RequesterOpenID, req.RequesterName, req.ChatID, req.MessageID, req.Tool, req.TaskID, req.TaskURL,
		req.SessionID, req.HeadCommit, req.PrHeadCommit, req.RetryReason, req.ResultText, req.Error, timeText(req.CreatedAt), timeText(req.UpdatedAt), nullableTimeText(req.CompletedAt),
		nullableTimeText(req.ArchivedAt), nullableTimeText(req.RefreshedAt), nullableTimeText(req.TaskSyncedAt)); err != nil {
		return ReviewRequest{}, err
	}
	if err := audit(ctx, tx, "requester:"+req.RequesterOpenID, "review.create", req.ID, mustJSON(req)); err != nil {
		return ReviewRequest{}, err
	}
	return req, tx.Commit()
}

func (s *Store) GetReview(id string) (ReviewRequest, error) {
	row := s.db.QueryRow(`
		SELECT id, thread_id, status, pr_url, repo, pr_number, title, base_branch,
			requester_open_id, requester_name, chat_id, message_id, tool, task_id, task_url,
			session_id, head_commit, pr_head_commit, retry_reason, result_text, error, created_at, updated_at, completed_at,
			archived_at, refreshed_at, task_synced_at
		FROM review_requests
		WHERE id = ?
	`, id)
	req, err := scanReviewRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRequest{}, ErrNotFound
	}
	if err != nil {
		return ReviewRequest{}, err
	}
	return req, nil
}

func (s *Store) GetReviewByPR(repo string, prNumber int) (ReviewRequest, error) {
	row := s.db.QueryRow(`
		SELECT id, thread_id, status, pr_url, repo, pr_number, title, base_branch,
			requester_open_id, requester_name, chat_id, message_id, tool, task_id, task_url,
			session_id, head_commit, pr_head_commit, retry_reason, result_text, error, created_at, updated_at, completed_at,
			archived_at, refreshed_at, task_synced_at
		FROM review_requests
		WHERE repo = ? AND pr_number = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, repo, prNumber)
	req, err := scanReviewRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRequest{}, ErrNotFound
	}
	if err != nil {
		return ReviewRequest{}, err
	}
	return req, nil
}

// GetLatestReviewByThread returns the most recent non-archived review whose
// thread is rooted at rootMessageID in chatID. It backs the "reply in a review
// thread" retry trigger.
func (s *Store) GetLatestReviewByThread(chatID, rootMessageID string) (ReviewRequest, error) {
	row := s.db.QueryRow(`
		SELECT r.id, r.thread_id, r.status, r.pr_url, r.repo, r.pr_number, r.title, r.base_branch,
			r.requester_open_id, r.requester_name, r.chat_id, r.message_id, r.tool, r.task_id, r.task_url,
			r.session_id, r.head_commit, r.pr_head_commit, r.retry_reason, r.result_text, r.error, r.created_at, r.updated_at, r.completed_at,
			r.archived_at, r.refreshed_at, r.task_synced_at
		FROM review_requests r
		JOIN review_threads t ON t.id = r.thread_id
		WHERE t.chat_id = ? AND t.root_message_id = ? AND r.status != 'archived'
		ORDER BY r.created_at DESC
		LIMIT 1
	`, chatID, rootMessageID)
	req, err := scanReviewRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRequest{}, ErrNotFound
	}
	if err != nil {
		return ReviewRequest{}, err
	}
	return req, nil
}

// ListReviews returns review requests, newest first. When status is empty every
// status is returned. Archived reviews are excluded by default (unless
// includeArchived is true or status is "archived"), so the default listings
// stay focused on active reviews.
func (s *Store) ListReviews(status string, includeArchived bool) []ReviewRequest {
	query := `
		SELECT id, thread_id, status, pr_url, repo, pr_number, title, base_branch,
			requester_open_id, requester_name, chat_id, message_id, tool, task_id, task_url,
			session_id, head_commit, pr_head_commit, retry_reason, result_text, error, created_at, updated_at, completed_at,
			archived_at, refreshed_at, task_synced_at
		FROM review_requests
	`
	var conds []string
	var args []any
	if status != "" {
		conds = append(conds, `status = ?`)
		args = append(args, status)
	}
	if !includeArchived && status != "archived" {
		conds = append(conds, `status != 'archived'`)
	}
	if len(conds) > 0 {
		query += ` WHERE ` + strings.Join(conds, " AND ")
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return []ReviewRequest{}
	}
	defer rows.Close()

	out := []ReviewRequest{}
	for rows.Next() {
		req, err := scanReviewRequest(rows)
		if err == nil {
			out = append(out, req)
		}
	}
	return out
}

func (s *Store) UpdateReview(req ReviewRequest) (ReviewRequest, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewRequest{}, err
	}
	defer rollback(tx)

	req.UpdatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE review_requests
		SET status = ?, pr_url = ?, repo = ?, pr_number = ?, title = ?, base_branch = ?,
			requester_open_id = ?, requester_name = ?, chat_id = ?, message_id = ?, tool = ?,
			task_id = ?, task_url = ?, session_id = ?, head_commit = ?, pr_head_commit = ?, retry_reason = ?, result_text = ?, error = ?, updated_at = ?, completed_at = ?,
			archived_at = ?, refreshed_at = ?, task_synced_at = ?
		WHERE id = ?
	`, req.Status, req.PRURL, req.Repo, req.PRNumber, req.Title, req.BaseBranch,
		req.RequesterOpenID, req.RequesterName, req.ChatID, req.MessageID, req.Tool,
		req.TaskID, req.TaskURL, req.SessionID, req.HeadCommit, req.PrHeadCommit, req.RetryReason, req.ResultText, req.Error, timeText(req.UpdatedAt), nullableTimeText(req.CompletedAt),
		nullableTimeText(req.ArchivedAt), nullableTimeText(req.RefreshedAt), nullableTimeText(req.TaskSyncedAt),
		req.ID); err != nil {
		return ReviewRequest{}, err
	}
	if err := audit(ctx, tx, "system", "review.update", req.ID, mustJSON(req)); err != nil {
		return ReviewRequest{}, err
	}
	return req, tx.Commit()
}

func (s *Store) FailInterruptedReviews() ([]string, error) {
	ctx := context.Background()
	rows, err := s.db.Query(`SELECT id FROM review_requests WHERE status = 'reviewing'`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	now := time.Now().UTC()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE review_requests SET status = 'failed', error = ?, updated_at = ?, completed_at = ? WHERE id = ?
		`, "interrupted by server restart", timeText(now), timeText(now), id); err != nil {
			return nil, err
		}
		if _, err := s.db.ExecContext(ctx, `
			UPDATE review_runs SET status = 'failed', error = ?, completed_at = ?, updated_at = ? WHERE review_id = ? AND status = 'running'
		`, "interrupted by server restart", timeText(now), timeText(now), id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// CreateReviewRun appends a run for a review, assigning the next sequence
// number (1 for the first run, 2, 3, ... for retries).
func (s *Store) CreateReviewRun(run ReviewRun) (ReviewRun, error) {
	ctx := context.Background()
	if run.ID == "" {
		run.ID = "run_" + newID()
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = run.StartedAt
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewRun{}, err
	}
	defer rollback(tx)

	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM review_runs WHERE review_id = ?`, run.ReviewID).Scan(&maxSeq); err != nil {
		return ReviewRun{}, err
	}
	run.Seq = int(maxSeq.Int64) + 1

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO review_runs (id, review_id, seq, status, head_commit, retry_reason, session_id, result_text, error, tool, started_at, completed_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, run.ID, run.ReviewID, run.Seq, run.Status, run.HeadCommit, run.RetryReason, run.SessionID, run.ResultText, run.Error, run.Tool,
		timeText(run.StartedAt), nullableTimeText(run.CompletedAt), timeText(run.UpdatedAt)); err != nil {
		return ReviewRun{}, err
	}
	return run, tx.Commit()
}

// UpdateReviewRun persists a run's terminal outcome (status, head commit,
// session, result/error).
func (s *Store) UpdateReviewRun(run ReviewRun) error {
	ctx := context.Background()
	run.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE review_runs
		SET status = ?, head_commit = ?, retry_reason = ?, session_id = ?, result_text = ?, error = ?, completed_at = ?, updated_at = ?
		WHERE id = ?
	`, run.Status, run.HeadCommit, run.RetryReason, run.SessionID, run.ResultText, run.Error,
		nullableTimeText(run.CompletedAt), timeText(run.UpdatedAt), run.ID)
	return err
}

// CurrentRun returns the in-flight run for a review (status 'running'), if any.
func (s *Store) CurrentRun(reviewID string) (ReviewRun, error) {
	row := s.db.QueryRow(`
		SELECT id, review_id, seq, status, head_commit, retry_reason, session_id, result_text, error, tool, started_at, completed_at, updated_at
		FROM review_runs
		WHERE review_id = ? AND status = 'running'
		ORDER BY seq DESC
		LIMIT 1
	`, reviewID)
	run, err := scanReviewRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRun{}, ErrNotFound
	}
	if err != nil {
		return ReviewRun{}, err
	}
	return run, nil
}

// ListRuns returns every run of a review in execution order (oldest first).
func (s *Store) ListRuns(reviewID string) []ReviewRun {
	rows, err := s.db.Query(`
		SELECT id, review_id, seq, status, head_commit, retry_reason, session_id, result_text, error, tool, started_at, completed_at, updated_at
		FROM review_runs
		WHERE review_id = ?
		ORDER BY seq ASC
	`, reviewID)
	if err != nil {
		return []ReviewRun{}
	}
	defer rows.Close()

	out := []ReviewRun{}
	for rows.Next() {
		run, err := scanReviewRun(rows)
		if err == nil {
			out = append(out, run)
		}
	}
	return out
}

// CreateReaction persists a new review reaction in status pending.
func (s *Store) CreateReaction(reac ReviewReaction) (ReviewReaction, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewReaction{}, err
	}
	defer rollback(tx)

	if _, err := getReviewTx(ctx, tx, reac.ReviewID); errors.Is(err, sql.ErrNoRows) {
		return ReviewReaction{}, ErrNotFound
	} else if err != nil {
		return ReviewReaction{}, err
	}
	if reac.ID == "" {
		reac.ID = "rxn_" + newID()
	}
	if reac.Status == "" {
		reac.Status = "pending"
	}
	if reac.CreatedAt.IsZero() {
		reac.CreatedAt = time.Now().UTC()
	}
	reac.UpdatedAt = reac.CreatedAt
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO review_reactions (
			id, review_id, instruction, status, chat_id, message_id, sender_open_id, sender_name,
			session_id, result_text, error, created_at, updated_at, completed_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, reac.ID, reac.ReviewID, reac.Instruction, reac.Status, reac.ChatID, reac.MessageID,
		reac.SenderOpenID, reac.SenderName, reac.SessionID, reac.ResultText, reac.Error,
		timeText(reac.CreatedAt), timeText(reac.UpdatedAt), nullableTimeText(reac.CompletedAt)); err != nil {
		return ReviewReaction{}, err
	}
	return reac, tx.Commit()
}

// GetReaction loads one review reaction by id.
func (s *Store) GetReaction(id string) (ReviewReaction, error) {
	row := s.db.QueryRow(`
		SELECT id, review_id, instruction, status, chat_id, message_id, sender_open_id, sender_name,
			session_id, result_text, error, created_at, updated_at, completed_at
		FROM review_reactions
		WHERE id = ?
	`, id)
	reac, err := scanReviewReaction(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewReaction{}, ErrNotFound
	}
	if err != nil {
		return ReviewReaction{}, err
	}
	return reac, nil
}

// ListReactions returns every reaction of a review in creation order (oldest
// first).
func (s *Store) ListReactions(reviewID string) []ReviewReaction {
	rows, err := s.db.Query(`
		SELECT id, review_id, instruction, status, chat_id, message_id, sender_open_id, sender_name,
			session_id, result_text, error, created_at, updated_at, completed_at
		FROM review_reactions
		WHERE review_id = ?
		ORDER BY created_at ASC
	`, reviewID)
	if err != nil {
		return []ReviewReaction{}
	}
	defer rows.Close()

	out := []ReviewReaction{}
	for rows.Next() {
		reac, err := scanReviewReaction(rows)
		if err == nil {
			out = append(out, reac)
		}
	}
	return out
}

// ListPendingReactions returns reactions still in status pending (used to
// re-enqueue them after a restart).
func (s *Store) ListPendingReactions() []ReviewReaction {
	rows, err := s.db.Query(`
		SELECT id, review_id, instruction, status, chat_id, message_id, sender_open_id, sender_name,
			session_id, result_text, error, created_at, updated_at, completed_at
		FROM review_reactions
		WHERE status = 'pending'
		ORDER BY created_at ASC
	`)
	if err != nil {
		return []ReviewReaction{}
	}
	defer rows.Close()

	out := []ReviewReaction{}
	for rows.Next() {
		reac, err := scanReviewReaction(rows)
		if err == nil {
			out = append(out, reac)
		}
	}
	return out
}

// UpdateReaction persists a reaction's mutable fields (status, session, result,
// error, timing).
func (s *Store) UpdateReaction(reac ReviewReaction) (ReviewReaction, error) {
	ctx := context.Background()
	reac.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE review_reactions
		SET instruction = ?, status = ?, chat_id = ?, message_id = ?, sender_open_id = ?, sender_name = ?,
			session_id = ?, result_text = ?, error = ?, updated_at = ?, completed_at = ?
		WHERE id = ?
	`, reac.Instruction, reac.Status, reac.ChatID, reac.MessageID, reac.SenderOpenID, reac.SenderName,
		reac.SessionID, reac.ResultText, reac.Error, timeText(reac.UpdatedAt), nullableTimeText(reac.CompletedAt),
		reac.ID)
	return reac, err
}

// FailInterruptedReactions marks reactions that were still running (status
// 'running') as failed after a server restart.
func (s *Store) FailInterruptedReactions() ([]string, error) {
	ctx := context.Background()
	rows, err := s.db.Query(`SELECT id FROM review_reactions WHERE status = 'running'`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	now := time.Now().UTC()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE review_reactions SET status = 'failed', error = ?, updated_at = ?, completed_at = ? WHERE id = ?
		`, "interrupted by server restart", timeText(now), timeText(now), id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

const (
	// TaskStatusOpen is a task bound to an open issue/effort.
	TaskStatusOpen = "open"
	// TaskStatusInProgress marks a task actively being worked on.
	TaskStatusInProgress = "in_progress"
	// TaskStatusDone marks a task whose issue is closed / PRs merged.
	TaskStatusDone = "done"
	// TaskStatusArchived hides a closed task from default listings.
	TaskStatusArchived = "archived"
)

func (s *Store) CreateTask(task Task) (Task, error) {
	ctx := context.Background()
	if task.ID == "" {
		task.ID = "tsk_" + newID()
	}
	if task.Status == "" {
		task.Status = TaskStatusOpen
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now().UTC()
	}
	task.UpdatedAt = task.CreatedAt
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tasks (id, title, status, issue_repo, issue_number, issue_title, issue_state, issue_url,
			description, archived_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, task.ID, task.Title, task.Status, task.IssueRepo, task.IssueNumber, task.IssueTitle, task.IssueState,
		task.IssueURL, task.Description, nullableTimeText(task.ArchivedAt), timeText(task.CreatedAt), timeText(task.UpdatedAt)); err != nil {
		return Task{}, err
	}
	if err := audit(ctx, tx, "system", "task.create", task.ID, mustJSON(task)); err != nil {
		return Task{}, err
	}
	return task, tx.Commit()
}

func (s *Store) GetTask(id string) (Task, error) {
	row := s.db.QueryRow(`
		SELECT id, title, status, issue_repo, issue_number, issue_title, issue_state, issue_url,
			description, archived_at, created_at, updated_at
		FROM tasks
		WHERE id = ?
	`, id)
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, err
	}
	return task, nil
}

// ListTasks returns tasks, newest first. When status is empty every status is
// returned. Archived tasks are excluded by default (unless includeArchived is
// true or status is "archived").
func (s *Store) ListTasks(status string, includeArchived bool) []Task {
	query := `
		SELECT id, title, status, issue_repo, issue_number, issue_title, issue_state, issue_url,
			description, archived_at, created_at, updated_at
		FROM tasks
	`
	var conds []string
	var args []any
	if status != "" {
		conds = append(conds, `status = ?`)
		args = append(args, status)
	}
	if !includeArchived && status != TaskStatusArchived {
		conds = append(conds, `status != 'archived'`)
	}
	if len(conds) > 0 {
		query += ` WHERE ` + strings.Join(conds, " AND ")
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return []Task{}
	}
	defer rows.Close()

	out := []Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err == nil {
			out = append(out, task)
		}
	}
	return out
}

func (s *Store) UpdateTask(task Task) (Task, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer rollback(tx)
	task.UpdatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET title = ?, status = ?, issue_repo = ?, issue_number = ?, issue_title = ?, issue_state = ?,
			issue_url = ?, description = ?, archived_at = ?, updated_at = ?
		WHERE id = ?
	`, task.Title, task.Status, task.IssueRepo, task.IssueNumber, task.IssueTitle, task.IssueState,
		task.IssueURL, task.Description, nullableTimeText(task.ArchivedAt), timeText(task.UpdatedAt), task.ID); err != nil {
		return Task{}, err
	}
	if err := audit(ctx, tx, "system", "task.update", task.ID, mustJSON(task)); err != nil {
		return Task{}, err
	}
	return task, tx.Commit()
}

// CloseTask archives a task: sets status archived and records archived_at.
func (s *Store) CloseTask(id string) (Task, error) {
	task, err := s.GetTask(id)
	if err != nil {
		return Task{}, err
	}
	if task.Status == TaskStatusArchived {
		return task, nil
	}
	task.Status = TaskStatusArchived
	task.ArchivedAt = time.Now().UTC()
	return s.UpdateTask(task)
}

func (s *Store) AddTaskDoc(doc TaskDoc) (TaskDoc, error) {
	ctx := context.Background()
	if _, err := s.GetTask(doc.TaskID); err != nil {
		return TaskDoc{}, err
	}
	if doc.ID == "" {
		doc.ID = "tdc_" + newID()
	}
	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO task_docs (id, task_id, repo, path, title, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_id, repo, path) DO UPDATE SET title = excluded.title
	`, doc.ID, doc.TaskID, doc.Repo, doc.Path, doc.Title, timeText(doc.CreatedAt))
	if err != nil {
		return TaskDoc{}, err
	}
	return doc, nil
}

func (s *Store) ListTaskDocs(taskID string) []TaskDoc {
	rows, err := s.db.Query(`
		SELECT id, task_id, repo, path, title, created_at
		FROM task_docs
		WHERE task_id = ?
		ORDER BY created_at ASC
	`, taskID)
	if err != nil {
		return []TaskDoc{}
	}
	defer rows.Close()

	out := []TaskDoc{}
	for rows.Next() {
		doc, err := scanTaskDoc(rows)
		if err == nil {
			out = append(out, doc)
		}
	}
	return out
}

func (s *Store) RemoveTaskDoc(id string) error {
	_, err := s.db.Exec(`DELETE FROM task_docs WHERE id = ?`, id)
	return err
}

// AddTaskPR inserts or upserts a linked PR (deduped by task_id+repo+pr_number).
// On conflict the PR fields and review snapshot are refreshed.
func (s *Store) AddTaskPR(pr TaskPR) (TaskPR, error) {
	ctx := context.Background()
	if _, err := s.GetTask(pr.TaskID); err != nil {
		return TaskPR{}, err
	}
	if pr.ID == "" {
		pr.ID = "tpr_" + newID()
	}
	if pr.ReviewStatus == "" {
		pr.ReviewStatus = "none"
	}
	if pr.CreatedAt.IsZero() {
		pr.CreatedAt = time.Now().UTC()
	}
	pr.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO task_prs (id, task_id, repo, pr_number, pr_url, pr_title, pr_state, base_branch,
			review_id, review_status, fs_task_id, fs_task_url, synced_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_id, repo, pr_number) DO UPDATE SET
			pr_url = excluded.pr_url, pr_title = excluded.pr_title, pr_state = excluded.pr_state,
			base_branch = excluded.base_branch, review_id = excluded.review_id,
			review_status = excluded.review_status,
			fs_task_id = COALESCE(NULLIF(excluded.fs_task_id, ''), fs_task_id),
			fs_task_url = COALESCE(NULLIF(excluded.fs_task_url, ''), fs_task_url),
			synced_at = COALESCE(excluded.synced_at, synced_at), updated_at = excluded.updated_at
	`, pr.ID, pr.TaskID, pr.Repo, pr.PRNumber, pr.PRURL, pr.PRTitle, pr.PRState, pr.BaseBranch,
		pr.ReviewID, pr.ReviewStatus, pr.FsTaskID, pr.FsTaskURL, nullableTimeText(pr.SyncedAt), timeText(pr.CreatedAt), timeText(pr.UpdatedAt))
	if err != nil {
		return TaskPR{}, err
	}
	return pr, nil
}

func (s *Store) GetTaskPR(id string) (TaskPR, error) {
	row := s.db.QueryRow(`
		SELECT id, task_id, repo, pr_number, pr_url, pr_title, pr_state, base_branch,
			review_id, review_status, fs_task_id, fs_task_url, synced_at, created_at, updated_at
		FROM task_prs
		WHERE id = ?
	`, id)
	pr, err := scanTaskPR(row)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskPR{}, ErrNotFound
	}
	if err != nil {
		return TaskPR{}, err
	}
	return pr, nil
}

func (s *Store) ListTaskPRs(taskID string) []TaskPR {
	rows, err := s.db.Query(`
		SELECT id, task_id, repo, pr_number, pr_url, pr_title, pr_state, base_branch,
			review_id, review_status, fs_task_id, fs_task_url, synced_at, created_at, updated_at
		FROM task_prs
		WHERE task_id = ?
		ORDER BY created_at ASC
	`, taskID)
	if err != nil {
		return []TaskPR{}
	}
	defer rows.Close()

	out := []TaskPR{}
	for rows.Next() {
		pr, err := scanTaskPR(rows)
		if err == nil {
			out = append(out, pr)
		}
	}
	return out
}

func (s *Store) UpdateTaskPR(pr TaskPR) (TaskPR, error) {
	ctx := context.Background()
	pr.UpdatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE task_prs
		SET pr_url = ?, pr_title = ?, pr_state = ?, base_branch = ?, review_id = ?,
			review_status = ?, fs_task_id = ?, fs_task_url = ?, synced_at = ?, updated_at = ?
		WHERE id = ?
	`, pr.PRURL, pr.PRTitle, pr.PRState, pr.BaseBranch, pr.ReviewID,
		pr.ReviewStatus, pr.FsTaskID, pr.FsTaskURL, nullableTimeText(pr.SyncedAt), timeText(pr.UpdatedAt), pr.ID)
	return pr, err
}

func (s *Store) RemoveTaskPR(id string) error {
	_, err := s.db.Exec(`DELETE FROM task_prs WHERE id = ?`, id)
	return err
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

type reviewThreadScanner interface {
	Scan(dest ...any) error
}

type reviewRequestScanner interface {
	Scan(dest ...any) error
}

type reviewRunScanner interface {
	Scan(dest ...any) error
}

type reviewReactionScanner interface {
	Scan(dest ...any) error
}

type taskScanner interface {
	Scan(dest ...any) error
}

type taskDocScanner interface {
	Scan(dest ...any) error
}

type taskPRScanner interface {
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

func getReviewThreadTx(ctx context.Context, tx *sql.Tx, id string) (ReviewThread, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT id, chat_id, root_message_id, topic, participants, last_message_at, created_at
		FROM review_threads
		WHERE id = ?
	`, id)
	return scanReviewThread(row)
}

func getReviewTx(ctx context.Context, tx *sql.Tx, id string) (ReviewRequest, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT id, thread_id, status, pr_url, repo, pr_number, title, base_branch,
			requester_open_id, requester_name, chat_id, message_id, tool, task_id, task_url,
			session_id, head_commit, pr_head_commit, retry_reason, result_text, error, created_at, updated_at, completed_at,
			archived_at, refreshed_at, task_synced_at
		FROM review_requests
		WHERE id = ?
	`, id)
	return scanReviewRequest(row)
}

func scanReviewThread(row reviewThreadScanner) (ReviewThread, error) {
	var thread ReviewThread
	var participants string
	var lastMessageAt string
	var createdAt string
	if err := row.Scan(&thread.ID, &thread.ChatID, &thread.RootMessageID, &thread.Topic, &participants, &lastMessageAt, &createdAt); err != nil {
		return ReviewThread{}, err
	}
	_ = json.Unmarshal([]byte(participants), &thread.Participants)
	thread.LastMessageAt, _ = parseTime(lastMessageAt)
	thread.CreatedAt, _ = parseTime(createdAt)
	return thread, nil
}

func scanReviewRequest(row reviewRequestScanner) (ReviewRequest, error) {
	var req ReviewRequest
	var createdAt string
	var updatedAt string
	var completedAt sql.NullString
	var archivedAt sql.NullString
	var refreshedAt sql.NullString
	var taskSyncedAt sql.NullString
	if err := row.Scan(
		&req.ID, &req.ThreadID, &req.Status, &req.PRURL, &req.Repo, &req.PRNumber, &req.Title, &req.BaseBranch,
		&req.RequesterOpenID, &req.RequesterName, &req.ChatID, &req.MessageID, &req.Tool, &req.TaskID, &req.TaskURL,
		&req.SessionID, &req.HeadCommit, &req.PrHeadCommit, &req.RetryReason, &req.ResultText, &req.Error, &createdAt, &updatedAt, &completedAt,
		&archivedAt, &refreshedAt, &taskSyncedAt,
	); err != nil {
		return ReviewRequest{}, err
	}
	req.CreatedAt, _ = parseTime(createdAt)
	req.UpdatedAt, _ = parseTime(updatedAt)
	req.CompletedAt, _ = parseNullTime(completedAt)
	req.ArchivedAt, _ = parseNullTime(archivedAt)
	req.RefreshedAt, _ = parseNullTime(refreshedAt)
	req.TaskSyncedAt, _ = parseNullTime(taskSyncedAt)
	return req, nil
}

func scanReviewRun(row reviewRunScanner) (ReviewRun, error) {
	var run ReviewRun
	var startedAt string
	var updatedAt string
	var completedAt sql.NullString
	if err := row.Scan(
		&run.ID, &run.ReviewID, &run.Seq, &run.Status, &run.HeadCommit, &run.RetryReason, &run.SessionID,
		&run.ResultText, &run.Error, &run.Tool, &startedAt, &completedAt, &updatedAt,
	); err != nil {
		return ReviewRun{}, err
	}
	run.StartedAt, _ = parseTime(startedAt)
	run.UpdatedAt, _ = parseTime(updatedAt)
	run.CompletedAt, _ = parseNullTime(completedAt)
	return run, nil
}

func scanReviewReaction(row reviewReactionScanner) (ReviewReaction, error) {
	var reac ReviewReaction
	var createdAt string
	var updatedAt string
	var completedAt sql.NullString
	if err := row.Scan(
		&reac.ID, &reac.ReviewID, &reac.Instruction, &reac.Status, &reac.ChatID, &reac.MessageID,
		&reac.SenderOpenID, &reac.SenderName, &reac.SessionID, &reac.ResultText, &reac.Error,
		&createdAt, &updatedAt, &completedAt,
	); err != nil {
		return ReviewReaction{}, err
	}
	reac.CreatedAt, _ = parseTime(createdAt)
	reac.UpdatedAt, _ = parseTime(updatedAt)
	reac.CompletedAt, _ = parseNullTime(completedAt)
	return reac, nil
}

func scanTask(row taskScanner) (Task, error) {
	var task Task
	var archivedAt sql.NullString
	var createdAt string
	var updatedAt string
	if err := row.Scan(
		&task.ID, &task.Title, &task.Status, &task.IssueRepo, &task.IssueNumber, &task.IssueTitle,
		&task.IssueState, &task.IssueURL, &task.Description, &archivedAt, &createdAt, &updatedAt,
	); err != nil {
		return Task{}, err
	}
	task.ArchivedAt, _ = parseNullTime(archivedAt)
	task.CreatedAt, _ = parseTime(createdAt)
	task.UpdatedAt, _ = parseTime(updatedAt)
	return task, nil
}

func scanTaskDoc(row taskDocScanner) (TaskDoc, error) {
	var doc TaskDoc
	var createdAt string
	if err := row.Scan(&doc.ID, &doc.TaskID, &doc.Repo, &doc.Path, &doc.Title, &createdAt); err != nil {
		return TaskDoc{}, err
	}
	doc.CreatedAt, _ = parseTime(createdAt)
	return doc, nil
}

func scanTaskPR(row taskPRScanner) (TaskPR, error) {
	var pr TaskPR
	var syncedAt sql.NullString
	var createdAt string
	var updatedAt string
	if err := row.Scan(
		&pr.ID, &pr.TaskID, &pr.Repo, &pr.PRNumber, &pr.PRURL, &pr.PRTitle, &pr.PRState, &pr.BaseBranch,
		&pr.ReviewID, &pr.ReviewStatus, &pr.FsTaskID, &pr.FsTaskURL, &syncedAt, &createdAt, &updatedAt,
	); err != nil {
		return TaskPR{}, err
	}
	pr.SyncedAt, _ = parseNullTime(syncedAt)
	pr.CreatedAt, _ = parseTime(createdAt)
	pr.UpdatedAt, _ = parseTime(updatedAt)
	return pr, nil
}

func mergeParticipants(existing, extra []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range append(append([]string{}, existing...), extra...) {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// GetChatMode returns the behavior mode for a chat. Chats without an explicit
// setting default to ChatModeReview.
func (s *Store) GetChatMode(chatID string) string {
	if chatID == "" {
		return ChatModeReview
	}
	row := s.db.QueryRow(`SELECT mode FROM chat_modes WHERE chat_id = ?`, chatID)
	var mode string
	if err := row.Scan(&mode); err != nil || mode == "" {
		return ChatModeReview
	}
	if normalized, err := NormalizeChatMode(mode); err == nil {
		return normalized
	}
	return mode
}

// SetChatMode upserts the behavior mode for a chat and returns the stored row.
// The mode string is normalized to its canonical flag-set form.
func (s *Store) SetChatMode(chatID, mode string) (ChatMode, error) {
	normalized, err := NormalizeChatMode(mode)
	if err != nil {
		return ChatMode{}, err
	}
	now := time.Now().UTC()
	if _, err := s.db.Exec(`
		INSERT INTO chat_modes (chat_id, mode, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET mode = excluded.mode, updated_at = excluded.updated_at
	`, chatID, normalized, timeText(now)); err != nil {
		return ChatMode{}, err
	}
	return ChatMode{ChatID: chatID, Mode: normalized, UpdatedAt: now}, nil
}

// ListChatModes returns every chat with an explicit mode, most recently
// updated first.
func (s *Store) ListChatModes() []ChatMode {
	rows, err := s.db.Query(`SELECT chat_id, mode, updated_at FROM chat_modes ORDER BY updated_at DESC`)
	if err != nil {
		return []ChatMode{}
	}
	defer rows.Close()
	var out []ChatMode
	for rows.Next() {
		var raw string
		var mode ChatMode
		if err := rows.Scan(&mode.ChatID, &mode.Mode, &raw); err != nil {
			continue
		}
		mode.UpdatedAt, _ = parseTime(raw)
		out = append(out, mode)
	}
	return out
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
