package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
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

type Database struct {
	Agents    map[string]Agent            `json:"agents"`
	Sessions  map[string]Session          `json:"sessions"`
	Events    map[string][]Event          `json:"events"`
	Approvals map[string]ApprovalRequest  `json:"approvals"`
	Decisions map[string]ApprovalDecision `json:"decisions"`
	AuditLogs []AuditLog                  `json:"audit_logs"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	db   Database
}

func Open(path string) (*Store, error) {
	s := &Store{path: path}
	s.db = Database{
		Agents:    map[string]Agent{},
		Sessions:  map[string]Session{},
		Events:    map[string][]Event{},
		Approvals: map[string]ApprovalRequest{},
		Decisions: map[string]ApprovalDecision{},
		AuditLogs: []AuditLog{},
	}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s.db); err != nil {
		return nil, err
	}
	s.ensureMaps()
	return s, nil
}

func (s *Store) ensureMaps() {
	if s.db.Agents == nil {
		s.db.Agents = map[string]Agent{}
	}
	if s.db.Sessions == nil {
		s.db.Sessions = map[string]Session{}
	}
	if s.db.Events == nil {
		s.db.Events = map[string][]Event{}
	}
	if s.db.Approvals == nil {
		s.db.Approvals = map[string]ApprovalRequest{}
	}
	if s.db.Decisions == nil {
		s.db.Decisions = map[string]ApprovalDecision{}
	}
	if s.db.AuditLogs == nil {
		s.db.AuditLogs = []AuditLog{}
	}
}

func (s *Store) RegisterAgentSession(agent Agent, session Session) (Agent, Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	if agent.ID == "" {
		agent.ID = "agt_" + newID()
	}
	existingAgent, ok := s.db.Agents[agent.ID]
	if ok && !existingAgent.CreatedAt.IsZero() {
		agent.CreatedAt = existingAgent.CreatedAt
	} else {
		agent.CreatedAt = now
	}
	agent.LastSeen = now
	s.db.Agents[agent.ID] = agent

	if session.ID == "" {
		session.ID = "ses_" + newID()
	}
	existingSession, ok := s.db.Sessions[session.ID]
	if ok && !existingSession.CreatedAt.IsZero() {
		session.CreatedAt = existingSession.CreatedAt
	} else {
		session.CreatedAt = now
	}
	if session.Status == "" {
		session.Status = "active"
	}
	session.AgentID = agent.ID
	session.UpdatedAt = now
	session.LastHeartbeatAt = now
	s.db.Sessions[session.ID] = session
	s.auditLocked("agent:"+agent.ID, "session.register", session.ID, mustJSON(session))
	return agent, session, s.saveLocked()
}

func (s *Store) Heartbeat(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	agent, ok := s.db.Agents[agentID]
	if !ok {
		return ErrNotFound
	}
	agent.LastSeen = now
	s.db.Agents[agentID] = agent
	for id, session := range s.db.Sessions {
		if session.AgentID == agentID && session.Status == "active" {
			session.LastHeartbeatAt = now
			session.UpdatedAt = now
			s.db.Sessions[id] = session
		}
	}
	return s.saveLocked()
}

func (s *Store) ListSessions() []Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Session, 0, len(s.db.Sessions))
	for _, session := range s.db.Sessions {
		out = append(out, session)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

func (s *Store) GetSession(id string) (Session, []Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.db.Sessions[id]
	if !ok {
		return Session{}, nil, ErrNotFound
	}
	events := append([]Event(nil), s.db.Events[id]...)
	sort.Slice(events, func(i, j int) bool {
		return events[i].CreatedAt.Before(events[j].CreatedAt)
	})
	return session, events, nil
}

func (s *Store) UpdateAutoPass(sessionID string, enabled bool, ttl time.Duration, scope string, actor string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.db.Sessions[sessionID]
	if !ok {
		return Session{}, ErrNotFound
	}
	session.AutoPass = enabled
	session.AutoPassScope = scope
	if enabled {
		session.AutoPassUntil = time.Now().UTC().Add(ttl)
	} else {
		session.AutoPassUntil = time.Time{}
	}
	session.UpdatedAt = time.Now().UTC()
	s.db.Sessions[sessionID] = session
	s.auditLocked(actor, "session.autopass", sessionID, mustJSON(session))
	return session, s.saveLocked()
}

func (s *Store) AddEvent(event Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.db.Sessions[event.SessionID]; !ok {
		return Event{}, ErrNotFound
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
	s.db.Events[event.SessionID] = append(s.db.Events[event.SessionID], event)
	session := s.db.Sessions[event.SessionID]
	session.UpdatedAt = event.CreatedAt
	if event.Type == "stop" {
		session.Status = "stopped"
	}
	s.db.Sessions[event.SessionID] = session
	s.auditLocked("agent:"+session.AgentID, "event.add", event.ID, mustJSON(event))
	return event, s.saveLocked()
}

func (s *Store) CreateApproval(req ApprovalRequest) (ApprovalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.db.Sessions[req.SessionID]; !ok {
		return ApprovalRequest{}, ErrNotFound
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
	s.db.Approvals[req.ID] = req
	s.auditLocked("agent:"+req.SessionID, "approval.create", req.ID, mustJSON(req))
	return req, s.saveLocked()
}

func (s *Store) ListApprovals(status string) []ApprovalRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ApprovalRequest, 0, len(s.db.Approvals))
	for _, req := range s.db.Approvals {
		if status == "" || req.Status == status {
			out = append(out, req)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

func (s *Store) DecideApproval(requestID string, decision ApprovalDecision) (ApprovalRequest, ApprovalDecision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.db.Approvals[requestID]
	if !ok {
		return ApprovalRequest{}, ApprovalDecision{}, ErrNotFound
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
	s.db.Approvals[requestID] = req
	s.db.Decisions[requestID] = decision
	s.auditLocked(decision.DecidedBy, "approval."+decision.Decision, requestID, mustJSON(decision))
	return req, decision, s.saveLocked()
}

func (s *Store) GetApproval(id string) (ApprovalRequest, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	req, ok := s.db.Approvals[id]
	return req, ok
}

func (s *Store) auditLocked(actor, action, target string, payload json.RawMessage) {
	s.db.AuditLogs = append(s.db.AuditLogs, AuditLog{
		ID:        "aud_" + newID(),
		Actor:     actor,
		Action:    action,
		Target:    target,
		Payload:   payload,
		CreatedAt: time.Now().UTC(),
	})
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	b, err := json.MarshalIndent(s.db, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
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
