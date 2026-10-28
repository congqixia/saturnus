// Package workerproto defines the control-plane protocol between the saturnus
// server and remote review workers over a single WebSocket connection.
//
// The connection is worker-initiated (outbound, NAT friendly). The server
// dispatches jobs by pushing "claim" frames; the worker reports liveness,
// progress and outcomes as events. All frames are JSON envelopes whose Type is
// a command (server -> worker) or event (worker -> server).
package workerproto

import (
	"encoding/json"
	"time"

	"saturnus/internal/ghinfo"
)

// Command types sent from the server to a worker.
const (
	CmdClaim  = "claim"
	CmdCancel = "cancel"
	CmdPing   = "ping"
)

// Event types sent from a worker to the server.
const (
	EvHello     = "hello"
	EvHeartbeat = "heartbeat"
	EvProgress  = "progress"
	EvStarted   = "started"
	EvComplete  = "complete"
	EvCancelAck = "cancel_ack"
	EvError     = "error"
)

// Job kinds dispatched to workers.
const (
	JobReview   = "review"
	JobReaction = "reaction"
)

// Frame is the envelope for every control message.
type Frame struct {
	// Type is a Cmd* or Ev* constant.
	Type string `json:"type"`
	// ReviewID names the review the message concerns (empty when irrelevant).
	ReviewID string `json:"review_id,omitempty"`
	// Payload is the type-specific JSON body.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Decode unmarshals the frame payload into dst.
func (f Frame) Decode(dst any) error {
	if len(f.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(f.Payload, dst)
}

// NewFrame builds a frame with a JSON-encoded payload.
func NewFrame(typ, reviewID string, payload any) Frame {
	raw, _ := json.Marshal(payload)
	return Frame{Type: typ, ReviewID: reviewID, Payload: raw}
}

// JobPayload is what a worker needs to execute one job (a review run or a
// reviewer reaction). It carries the server-resolved context; the worker fills
// in its own local checkout path and renders the command template itself.
type JobPayload struct {
	// Kind is JobReview or JobReaction.
	Kind       string `json:"kind"`
	ReviewID   string `json:"review_id"`
	ReactionID string `json:"reaction_id,omitempty"`
	RunID      string `json:"run_id,omitempty"`

	// PR context (shared by review and reaction jobs).
	Repo       string `json:"repo"`
	PRNumber   int    `json:"pr_number"`
	PRURL      string `json:"pr_url"`
	Title      string `json:"title,omitempty"`
	BaseBranch string `json:"base_branch,omitempty"`
	HeadCommit string `json:"head_commit,omitempty"`

	// Review-run fields.
	RetryReason     string `json:"retry_reason,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	PriorFindings   string `json:"prior_findings,omitempty"`
	CommandTemplate string `json:"command_template,omitempty"`
	Guide           string `json:"guide,omitempty"`
	ResultAPIURL    string `json:"result_api_url,omitempty"`
	ResultToken     string `json:"result_token,omitempty"`

	// Reaction fields.
	Instruction string `json:"instruction,omitempty"`

	// Tool is the review executable on the worker host; Timeout bounds the job.
	Tool    string        `json:"tool,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"`
}

// HelloPayload is sent once on connect so the server can reconcile the jobs
// the worker is still running after a reconnect.
type HelloPayload struct {
	WorkerID string   `json:"worker_id"`
	Token    string   `json:"token"`
	InFlight []string `json:"in_flight,omitempty"`
}

// HeartbeatPayload reports liveness and current load.
type HeartbeatPayload struct {
	Load     int          `json:"load,omitempty"`
	InFlight int          `json:"in_flight,omitempty"`
	ReposOK  int          `json:"repos_ok,omitempty"`
	GitHub   *ghinfo.Info `json:"github,omitempty"`
}

// ProgressPayload is a chunk of a job's streamed output.
type ProgressPayload struct {
	Text string `json:"text"`
}

// StartedPayload reports that the worker accepted the job and began executing
// it. Optional run_id keeps old workers and servers compatible.
type StartedPayload struct {
	RunID      string `json:"run_id,omitempty"`
	ReactionID string `json:"reaction_id,omitempty"`
}

// CompletePayload reports a job's outcome.
type CompletePayload struct {
	RunID      string `json:"run_id,omitempty"`
	OK         bool   `json:"ok"`
	ExitCode   int    `json:"exit_code,omitempty"`
	HeadCommit string `json:"head_commit,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	// ReactionID is set when the completed job was a reviewer reaction; the
	// server routes the completion to the reaction state machine accordingly.
	ReactionID string `json:"reaction_id,omitempty"`
	ResultText string `json:"result_text,omitempty"`
	Error      string `json:"error,omitempty"`
}

// CancelPayload asks a worker to stop a job.
type CancelPayload struct {
	Reason string `json:"reason,omitempty"`
}

// ErrorPayload reports a protocol-level failure; Code distinguishes recoverable
// conditions (e.g. "busy" when a worker is at capacity).
type ErrorPayload struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}
