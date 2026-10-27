package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"saturnus/internal/ghinfo"
)

// RoleReviewWorker marks an agent as a remote review executor.
const RoleReviewWorker = "review-worker"

// WorkerCapabilities describes what a review worker can execute: the repos it
// has checked out locally (owner/repo -> checkout path), the review tool it
// runs, whether gh is available, and how many jobs it runs concurrently. It is
// stored JSON-encoded on agents.capabilities.
type WorkerCapabilities struct {
	Repos         map[string]string `json:"repos"`
	Tool          string            `json:"tool"`
	ToolVersion   string            `json:"tool_version"`
	GH            bool              `json:"gh"`
	GitHub        *ghinfo.Info      `json:"github,omitempty"`
	MaxConcurrent int               `json:"max_concurrent"`
}

// UpdateWorkerGitHub refreshes metadata from an authenticated control channel.
// A stale heartbeat cannot replace a newer registration or capability set.
func (s *Store) UpdateWorkerGitHub(workerID string, info *ghinfo.Info) (bool, error) {
	if info == nil {
		return false, nil
	}
	agent, err := s.GetWorker(workerID)
	if err != nil {
		return false, err
	}
	caps, err := ParseWorkerCapabilities(agent.Capabilities)
	if err != nil {
		return false, err
	}
	if caps.GitHub != nil && !info.CheckedAt.After(caps.GitHub.CheckedAt) {
		return false, nil
	}
	caps.GitHub = info
	raw, err := caps.EncodeJSON()
	if err != nil {
		return false, err
	}
	result, err := s.db.Exec(`UPDATE agents SET capabilities=? WHERE id=? AND role=? AND capabilities=?`, raw, workerID, RoleReviewWorker, agent.Capabilities)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// RepoList returns the worker's checked-out repo names.
func (c WorkerCapabilities) RepoList() []string {
	out := make([]string, 0, len(c.Repos))
	for repo := range c.Repos {
		out = append(out, repo)
	}
	return out
}

// EncodeJSON encodes the capabilities for storage.
func (c WorkerCapabilities) EncodeJSON() (string, error) {
	data, err := json.Marshal(c)
	return string(data), err
}

// ParseWorkerCapabilities decodes a stored capabilities blob.
func ParseWorkerCapabilities(raw string) (WorkerCapabilities, error) {
	var caps WorkerCapabilities
	if raw == "" {
		return caps, nil
	}
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		return caps, err
	}
	return caps, nil
}

// RegisterWorker upserts a review-worker agent and its capabilities. A provided
// worker token is set on first registration and kept on later re-registrations,
// so a worker reconnecting keeps its identity. The returned Agent carries the
// effective token so callers can authenticate the control channel.
func (s *Store) RegisterWorker(agent Agent, caps WorkerCapabilities) (Agent, error) {
	ctx := context.Background()
	if agent.ID == "" {
		agent.ID = "wrk_" + newID()
	}
	now := time.Now().UTC()
	agent.Role = RoleReviewWorker
	agent.LastSeen = now
	capsJSON, err := caps.EncodeJSON()
	if err != nil {
		return Agent{}, err
	}
	agent.Capabilities = capsJSON
	if agent.Type == "" {
		agent.Type = "review-worker"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Agent{}, err
	}
	defer rollback(tx)
	createdAt, err := getCreatedAt(ctx, tx, "agents", agent.ID)
	if err != nil {
		return Agent{}, err
	}
	if createdAt.IsZero() {
		agent.CreatedAt = now
	} else {
		agent.CreatedAt = createdAt
	}
	if err := upsertAgentTx(ctx, tx, agent); err != nil {
		return Agent{}, err
	}
	// Re-read the effective token (kept across re-registrations when unset).
	var token string
	if err := tx.QueryRowContext(ctx, `SELECT worker_token FROM agents WHERE id = ?`, agent.ID).Scan(&token); err != nil {
		return Agent{}, err
	}
	agent.WorkerToken = token
	if err := audit(ctx, tx, "worker:"+agent.ID, "worker.register", agent.ID, mustJSON(agent)); err != nil {
		return Agent{}, err
	}
	return agent, tx.Commit()
}

// SetWorkerToken assigns a worker's authentication token (used after a shared
// register key has been validated).
func (s *Store) SetWorkerToken(workerID, token string) error {
	if workerID == "" || token == "" {
		return errors.New("empty worker id or token")
	}
	result, err := s.db.Exec(`UPDATE agents SET worker_token = ? WHERE id = ? AND role = ?`, token, workerID, RoleReviewWorker)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetWorker loads one review worker by id.
func (s *Store) GetWorker(id string) (Agent, error) {
	row := s.db.QueryRow(`SELECT id, name, type, host, version, role, capabilities, worker_token, last_seen, created_at
		FROM agents WHERE id = ?`, id)
	agent, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	if err != nil {
		return Agent{}, err
	}
	if agent.Role != RoleReviewWorker {
		return Agent{}, ErrNotFound
	}
	return agent, nil
}

// ListWorkers returns every review worker, most recently seen first.
func (s *Store) ListWorkers() []Agent {
	workers, err := s.ListWorkersWithError()
	if err != nil {
		return []Agent{}
	}
	return workers
}

// ListWorkersWithError lets background monitoring report storage failures.
func (s *Store) ListWorkersWithError() ([]Agent, error) {
	rows, err := s.db.Query(`SELECT id, name, type, host, version, role, capabilities, worker_token, last_seen, created_at
		FROM agents WHERE role = ? ORDER BY last_seen DESC`, RoleReviewWorker)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, agent)
	}
	return out, rows.Err()
}

// ListOnlineWorkers returns review workers whose last heartbeat is within
// onlineWindow of now.
func (s *Store) ListOnlineWorkers(onlineWindow time.Duration) []Agent {
	cutoff := timeText(time.Now().UTC().Add(-onlineWindow))
	rows, err := s.db.Query(`SELECT id, name, type, host, version, role, capabilities, worker_token, last_seen, created_at
		FROM agents WHERE role = ? AND last_seen >= ? ORDER BY last_seen DESC`, RoleReviewWorker, cutoff)
	if err != nil {
		return []Agent{}
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		if agent, err := scanAgent(rows); err == nil {
			out = append(out, agent)
		}
	}
	return out
}

// ErrNotPending is returned when an assign/requeue transition fails because the
// record is not in the expected state (or is owned by another worker).
var ErrNotPending = errors.New("record is not in the expected state for the transition")

// AssignReview atomically moves a pending review to reviewing and binds it to a
// worker. The transition is conditional on status='pending' so two dispatchers
// cannot claim the same review.
func (s *Store) AssignReview(reviewID, workerID string) (ReviewRequest, error) {
	ctx := context.Background()
	now := timeText(time.Now().UTC())
	result, err := s.db.ExecContext(ctx, `
		UPDATE review_requests SET status = 'reviewing', worker_id = ?, updated_at = ? WHERE id = ? AND status = 'pending'
	`, workerID, now, reviewID)
	if err != nil {
		return ReviewRequest{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return ReviewRequest{}, err
	}
	if n == 0 {
		// Distinguish missing vs not-pending for callers.
		if _, err := s.GetReview(reviewID); errors.Is(err, ErrNotFound) {
			return ReviewRequest{}, ErrNotFound
		}
		return ReviewRequest{}, ErrNotPending
	}
	return s.GetReview(reviewID)
}

// RequeueReview returns a reviewing review owned by workerID to pending,
// clearing the worker binding so it can be dispatched to another worker.
func (s *Store) RequeueReview(reviewID, workerID string) error {
	result, err := s.db.Exec(`
		UPDATE review_requests SET status = 'pending', worker_id = '', updated_at = ? WHERE id = ? AND status = 'reviewing' AND worker_id = ?
	`, timeText(time.Now().UTC()), reviewID, workerID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotPending
	}
	return nil
}

// ListReviewing returns every review currently in reviewing state.
func (s *Store) ListReviewing() []ReviewRequest {
	return s.ListReviews("reviewing", false)
}

// UpdateReviewOutcomeByWorker persists the terminal status of a review run
// reported by a remote worker, gated on the review still being owned by that
// worker. It also records the reviewed head commit and captured session, and
// clears the retry reason (mirroring the local finish path). It reports whether
// the update applied (false when the review was already reassigned, archived or
// otherwise changed).
func (s *Store) UpdateReviewOutcomeByWorker(reviewID, workerID, status, headCommit, sessionID, resultText, errorText string, completedAt time.Time) (bool, error) {
	now := timeText(time.Now().UTC())
	result, err := s.db.Exec(`
		UPDATE review_requests SET status = ?, head_commit = ?, pr_head_commit = ?, session_id = ?, retry_reason = '', result_text = ?, error = ?, completed_at = ?, updated_at = ?
		WHERE id = ? AND status = 'reviewing' AND worker_id = ?
	`, status, headCommit, headCommit, sessionID, resultText, errorText, nullableTimeText(completedAt), now, reviewID, workerID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// AssignReaction atomically moves a pending reaction to running and binds it to
// a worker.
func (s *Store) AssignReaction(reactionID, workerID string) (ReviewReaction, error) {
	result, err := s.db.Exec(`
		UPDATE review_reactions SET status = 'running', worker_id = ?, updated_at = ? WHERE id = ? AND status = 'pending'
	`, workerID, timeText(time.Now().UTC()), reactionID)
	if err != nil {
		return ReviewReaction{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return ReviewReaction{}, err
	}
	if n == 0 {
		if _, err := s.GetReaction(reactionID); errors.Is(err, ErrNotFound) {
			return ReviewReaction{}, ErrNotFound
		}
		return ReviewReaction{}, ErrNotPending
	}
	return s.GetReaction(reactionID)
}

// UpdateReactionOutcomeByWorker persists a reaction's terminal status, gated on
// it still being owned by the worker. It also records the captured session.
// Reports whether the update applied.
func (s *Store) UpdateReactionOutcomeByWorker(reactionID, workerID, status, sessionID, resultText, errorText string, completedAt time.Time) (bool, error) {
	result, err := s.db.Exec(`
		UPDATE review_reactions SET status = ?, session_id = ?, result_text = ?, error = ?, completed_at = ?, updated_at = ?
		WHERE id = ? AND status = 'running' AND worker_id = ?
	`, status, sessionID, resultText, errorText, nullableTimeText(completedAt), timeText(time.Now().UTC()), reactionID, workerID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// RequeueReaction returns a running reaction owned by workerID to pending,
// clearing the worker binding.
func (s *Store) RequeueReaction(reactionID, workerID string) error {
	result, err := s.db.Exec(`
		UPDATE review_reactions SET status = 'pending', worker_id = '', updated_at = ? WHERE id = ? AND status = 'running' AND worker_id = ?
	`, timeText(time.Now().UTC()), reactionID, workerID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotPending
	}
	return nil
}

// LastRunWorkerID returns the worker that executed the most recent run of a
// review (empty when no run yet, or every run was local). The scheduler uses it
// for session affinity when re-dispatching a retry after a requeue, so the new
// run resumes the previous tool session on the same host.
func (s *Store) LastRunWorkerID(reviewID string) string {
	var id string
	err := s.db.QueryRow(`
		SELECT worker_id FROM review_runs
		WHERE review_id = ? AND worker_id != ''
		ORDER BY seq DESC LIMIT 1
	`, reviewID).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

// ListRunningReactions returns reactions currently in running state.
func (s *Store) ListRunningReactions() []ReviewReaction {
	rows, err := s.db.Query(`SELECT id, review_id, instruction, status, chat_id, message_id, sender_open_id, sender_name,
		session_id, worker_id, result_text, error, created_at, updated_at, completed_at
		FROM review_reactions WHERE status = 'running' ORDER BY created_at ASC`)
	if err != nil {
		return []ReviewReaction{}
	}
	defer rows.Close()
	out := []ReviewReaction{}
	for rows.Next() {
		if reac, err := scanReviewReaction(rows); err == nil {
			out = append(out, reac)
		}
	}
	return out
}

func upsertAgentTx(ctx context.Context, tx *sql.Tx, agent Agent) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO agents (id, name, type, host, version, role, capabilities, worker_token, last_seen, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			type = excluded.type,
			host = excluded.host,
			version = excluded.version,
			role = excluded.role,
			capabilities = excluded.capabilities,
			worker_token = CASE WHEN excluded.worker_token = '' THEN agents.worker_token ELSE excluded.worker_token END,
			last_seen = excluded.last_seen
	`, agent.ID, agent.Name, agent.Type, agent.Host, agent.Version, agent.Role, agent.Capabilities, agent.WorkerToken, timeText(agent.LastSeen), timeText(agent.CreatedAt))
	return err
}

func scanAgent(row agentScanner) (Agent, error) {
	var agent Agent
	var lastSeen, createdAt string
	if err := row.Scan(&agent.ID, &agent.Name, &agent.Type, &agent.Host, &agent.Version, &agent.Role,
		&agent.Capabilities, &agent.WorkerToken, &lastSeen, &createdAt); err != nil {
		return Agent{}, err
	}
	agent.LastSeen, _ = parseTime(lastSeen)
	agent.CreatedAt, _ = parseTime(createdAt)
	return agent, nil
}

type agentScanner interface {
	Scan(dest ...any) error
}

// WorkerOnline reports whether an agent's last_seen is within window of now.
func WorkerOnline(agent Agent, window time.Duration) bool {
	return !agent.LastSeen.IsZero() && time.Since(agent.LastSeen) < window
}

// Capabilities returns the worker's decoded capabilities.
func (a Agent) CapabilitiesDecoded() (WorkerCapabilities, error) {
	return ParseWorkerCapabilities(a.Capabilities)
}

// String is a compact debug representation of a worker.
func (a Agent) String() string {
	return fmt.Sprintf("%s(%s)", a.ID, a.Host)
}
