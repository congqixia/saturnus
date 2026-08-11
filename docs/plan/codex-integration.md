# Codex Integration Guide

## What This Adds

Saturnus can be attached to Codex through Codex hooks. The adapter is
`saturnus-agent codex-hook`; it reads the hook JSON from stdin, registers the
Codex session with the Saturnus server, uploads timeline events, and turns
Saturnus approval decisions into Codex `PermissionRequest` decisions.

The repo contains a ready-to-use hook config at `.codex/hooks.json`.

## Build the Hook Adapter

```sh
./scripts/install-codex-hook.sh
```

This builds:

```text
bin/saturnus-agent
```

Codex calls:

```text
scripts/saturnus-codex-hook.sh
```

The wrapper locates `bin/saturnus-agent`, sets default Saturnus environment
variables, and exits with `{}` if the binary has not been built yet.

## Start Saturnus

Build the frontend once:

```sh
cd web
npm install
npm run build
cd ..
```

Start the server:

```sh
go run ./cmd/saturnus-server -addr :8787
```

Open:

```text
http://localhost:8787
```

## Enable the Repo-local Codex Hook

The checked-in `.codex/hooks.json` expects:

```text
bin/saturnus-agent
```

Run Codex from the repository root after building the hook adapter.

Useful environment variables:

```sh
export SATURNUS_SERVER=http://localhost:8787
export SATURNUS_CODEX_APPROVAL_WAIT=9m
```

`SATURNUS_CODEX_APPROVAL_WAIT` should be lower than the Codex hook timeout for
`PermissionRequest`; the included config uses a 600 second hook timeout and a
default 9 minute Saturnus polling window.

## Approval Flow

1. Codex starts a session.
2. `SessionStart` calls `saturnus-agent codex-hook`.
3. The adapter registers the Codex session in Saturnus.
4. Codex asks for tool permission.
5. `PermissionRequest` creates a Saturnus approval request.
6. Saturnus either auto-approves low-risk requests or waits for a Web/Lark
   decision.
7. The hook returns `allow` or `deny` to Codex when Saturnus has a decision.
8. If Saturnus is unavailable or no decision arrives before the wait window, the
   hook returns `{}` so Codex falls back to its native approval flow.

## Auto-pass Mode

There are two supported ways to enable auto-pass.

From the web UI:

1. Open the session.
2. Click `Auto-pass On`.
3. Saturnus enables a 30 minute session-scoped auto-pass window.

From the Codex process environment:

```sh
export SATURNUS_AUTO_PASS=1
```

When `SATURNUS_AUTO_PASS=1`, the hook registers new Codex sessions with
auto-pass enabled. Saturnus still applies its policy guardrails:

- low-risk read/write/test commands can be approved automatically
- medium or high risk requests still require manual approval
- every automatic decision is stored as an approval decision and audit log

## Lark Review

If Lark credentials are configured, pending approvals can be handled with bot
commands:

```text
/sat pending
/sat approve <request_id>
/sat deny <request_id> <reason>
/sat autopass on <session_id>
/sat autopass off <session_id>
```

## Hook Behavior

The adapter handles these Codex events:

- `SessionStart`: register or update the Saturnus session
- `UserPromptSubmit`: append a timeline event
- `PermissionRequest`: create an approval and return `allow` or `deny` when
  Saturnus has a decision
- `PostToolUse`: append a timeline event
- `Stop`: append a timeline event

The adapter logs failures to stderr and returns an empty JSON object when it
cannot safely decide. That keeps Codex usable even if Saturnus is down.
