# Saturnus MVP Plan

## Goal

Saturnus is a local-first control plane for AI CLI sessions. It records agent
sessions, exposes pending approval dialogs, integrates with Lark, acts as a Lark
bot, and provides a web UI for querying and handling sessions.

## MVP Scope

- Go single binary server.
- Embedded web UI served by the backend.
- JSON file persistence for local development.
- Agent registration, heartbeat, event reporting, and approval decisions.
- Session-scoped auto-pass policy with bounded risk.
- Lark webhook endpoint with challenge handling and text command handling.
- Lark approval notifications through message send API when credentials exist.
- Generic hook CLI that AI CLI hooks can call through stdin JSON or flags.

## Preferred Future Stack

- Backend: Go, `chi` or `gin` when dependencies are allowed.
- Database: SQLite for single-node use, Postgres for team deployment.
- Frontend: Vue 3 + Vite + Pinia + Element Plus or Naive UI.
- Realtime: SSE for session updates, WebSocket later if interactive streaming
  becomes necessary.
- Lark: official OpenAPI plus optional SDK wrapper.
- Agent integration: Codex hooks first, then generic adapters for other AI CLIs.

## Runtime Components

### Server

The server owns the core API and serves the frontend. It records:

- agents
- sessions
- events
- approval requests
- approval decisions
- audit logs
- Lark chat bindings

### Agent Adapter

The adapter is a small CLI that AI CLI hooks can execute. It supports:

- `register`: create or update an agent session.
- `event`: append an event to a session timeline.
- `approval`: request approval for a tool/action and wait for a decision.
- `heartbeat`: keep an agent/session marked active.

For Codex, the adapter maps hook payloads such as `SessionStart`,
`PreToolUse`, `PostToolUse`, `PermissionRequest`, and `Stop` into Saturnus API
calls.

### Web UI

The web UI provides:

- session list
- session detail timeline
- pending approvals
- approve/deny controls
- session auto-pass toggle
- agent online/last-seen status

### Lark Bot

The bot supports text commands:

- `/sat sessions`
- `/sat pending`
- `/sat session <session_id>`
- `/sat approve <request_id>`
- `/sat deny <request_id> <reason>`
- `/sat autopass on <session_id>`
- `/sat autopass off <session_id>`

Interactive cards can be added after the first API loop is stable.

## Auto-pass Policy

Auto-pass must be bounded rather than globally unconditional.

- It is scoped to one session.
- It has a TTL.
- It records every automatic decision in the audit log.
- It allows low-risk requests only:
  - read-only actions
  - workspace-local writes
  - explicit command allowlist prefixes
- It denies or requires manual review for high-risk requests:
  - destructive filesystem commands
  - writes outside the workspace
  - deploy, publish, or git push
  - secret, token, or env exfiltration risks
  - arbitrary network calls that include sensitive payloads

## MVP Data Storage

The first version now uses SQLite, defaulting to `saturnus.db`. The storage
layer is isolated under `internal/store` so Postgres can replace it later if
team deployment needs it.

## API Sketch

- `GET /api/health`
- `POST /api/agents/register`
- `POST /api/agents/{agent_id}/heartbeat`
- `GET /api/sessions`
- `GET /api/sessions/{session_id}`
- `PATCH /api/sessions/{session_id}/autopass`
- `POST /api/sessions/{session_id}/events`
- `GET /api/approvals?status=pending`
- `POST /api/approvals`
- `POST /api/approvals/{request_id}/decision`
- `POST /lark/events`

## Implementation Milestones

1. Server, JSON store, and core API.
2. Web UI over the core API.
3. Lark webhook commands.
4. Agent hook CLI.
5. SSE updates and interactive cards.
6. SQLite/Postgres storage and authentication.
