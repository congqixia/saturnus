# Saturnus

Saturnus is an MVP control plane for AI CLI sessions. It provides a Go server,
a web UI, agent session APIs, approval tracking, bounded auto-pass policy, and a
Lark bot webhook.

## Run

Build the frontend first:

```sh
cd web
npm install
npm run build
cd ..
```

```sh
go run ./cmd/saturnus-server -addr :8787
```

Open:

```text
http://localhost:8787
```

Data is stored in `saturnus.db` by default.

For frontend development:

```sh
cd web
npm run dev
```

The Vite dev server proxies `/api` and `/lark` to `http://localhost:8787`.

## Agent CLI

Register a session:

```sh
go run ./cmd/saturnus-agent register
```

Send an event:

```sh
echo '{"type":"UserPromptSubmit","payload":{"prompt":"hello"}}' \
  | go run ./cmd/saturnus-agent -session <session_id> event
```

Request approval and wait for a decision:

```sh
echo '{"tool_name":"exec_command","tool_input":{"cmd":"go test ./..."}}' \
  | go run ./cmd/saturnus-agent -session <session_id> approval
```

Enable auto-pass for a session:

```sh
curl -X PATCH http://localhost:8787/api/sessions/<session_id>/autopass \
  -H 'Content-Type: application/json' \
  -d '{"enabled":true,"ttl":"30m","scope":"session","actor":"operator"}'
```

Auto-pass is intentionally bounded. Only low-risk requests are approved
automatically.

## Storage

The MVP uses SQLite through `internal/store`. The server creates and migrates
the schema on startup. Use `-data <path>` to point at a different database file.

## Lark Bot

Set credentials before starting the server:

```sh
export LARK_APP_ID=cli_xxx
export LARK_APP_SECRET=xxx
export LARK_NOTIFY_CHAT_ID=oc_xxx
go run ./cmd/saturnus-server -addr :8787
```

Configure the Lark event callback URL to:

```text
https://<public-domain>/lark/events
```

Supported text commands:

- `/sat sessions`
- `/sat pending`
- `/sat session <session_id>`
- `/sat approve <request_id>`
- `/sat deny <request_id> <reason>`
- `/sat autopass on <session_id>`
- `/sat autopass off <session_id>`

## Codex Hook Direction

Use `saturnus-agent codex-hook` as the bridge command. It reads hook JSON from
stdin and maps `SessionStart`, `PermissionRequest`, and other hook events into
Saturnus API calls.

Build the local hook binary:

```sh
./scripts/install-codex-hook.sh
```

For a practical setup, export:

```sh
export SATURNUS_SERVER=http://localhost:8787
export SATURNUS_CODEX_APPROVAL_WAIT=9m
```

The repo-local Codex hook config is checked in at:

```text
.codex/hooks.json
```

It invokes:

```text
scripts/saturnus-codex-hook.sh
```

See `docs/plan/codex-integration.md` for the full setup and behavior guide.
