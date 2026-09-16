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

Build and run the server binary:

```sh
make run-server
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
make run-server
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
- `/sat reviews`
- `/sat review <pr-url>` (also works as natural language: any message containing a GitHub PR URL)
- `/sat review <review_id>`
- `/sat repos` — show the configured repo whitelist
- `/sat whoami` — show the sender's own open_id
- `/sat whois <name|email|mobile>` — resolve a user to open_id via the contact API
- `/sat reply <review_id> <message>` — reviewer replies to the requester via DM

## PR Review

The server can accept PR review requests from the Lark bot conversation, record
the review request together with its thread context, optionally create a Feishu
task, and run a local tool (default `opencode`) to review the PR in a detached
worktree. The result is sent to a fixed reviewer.

Enable it with:

```sh
export SATURNUS_REVIEW_ENABLED=1
export SATURNUS_REVIEW_ALLOWED_USERS=ou_xxx,ou_yyy     # senders who may request reviews
export SATURNUS_REVIEW_REVIEWER_OPEN_ID=ou_reviewer    # who receives the result (or REVIEWER_CHAT_ID)
export SATURNUS_REVIEW_REPOS='owner/repo=/path/to/local/checkout,owner2/repo2=/path/to/other'
export SATURNUS_REVIEW_CREATE_TASK=1                   # optional: create a Feishu task
export SATURNUS_REVIEW_REPLY_IN_THREAD=1               # optional: also reply in the original thread
make run-server
```

`SATURNUS_REVIEW_REPOS` is a comma-separated whitelist of `owner/repo=/local/path`
pairs. Only repos listed here can be reviewed; anything else is rejected
immediately with "repo is not in the review whitelist". Each mapped path must be
a git checkout. The server does not fetch or check out anything itself: it runs
the review tool directly inside the mapped checkout and lets the tool fetch and
check out the PR head on its own, so the shared checkout is left untouched by
Saturnus.

Environment variables:

| variable | default | note |
|---|---|---|
| `SATURNUS_REVIEW_ENABLED` | 0 | master switch; set to `1` to enable |
| `SATURNUS_REVIEW_ALLOWED_USERS` | - | comma-separated open_ids; `*` = allow everyone |
| `SATURNUS_REVIEW_REPOS` | - | whitelist `owner/repo=/local/path,...` |
| `SATURNUS_REVIEW_REVIEWER_OPEN_ID` | - | DM recipient for results |
| `SATURNUS_REVIEW_REVIEWER_CHAT_ID` | - | chat recipient fallback |
| `SATURNUS_REVIEW_REPLY_IN_THREAD` | 0 | also reply into the original thread |
| `SATURNUS_REVIEW_TOOL` | opencode | review executable |
| `SATURNUS_REVIEW_COMMAND_TEMPLATE` | see below | `text/template` for command args |
| `SATURNUS_REVIEW_MAX_CONCURRENT` | 1 | worker count |
| `SATURNUS_REVIEW_TIMEOUT` | 15m | per-review timeout |
| `SATURNUS_REVIEW_CREATE_TASK` | 0 | create a Feishu task assigned to reviewer + requester |
| `SATURNUS_REVIEW_TASK_DUE_HOURS` | 24 | task due offset in hours |
| `SATURNUS_REVIEW_LOG_DIR` | `<tmp>/saturnus-review-logs` | per-review transcript logs |
| `SATURNUS_REVIEW_GUIDES` | - | per-repo review guide files: `owner/repo=/path/to/guide.txt,...` |

While a review is running, the tool output is streamed: the accumulated text is
flushed into `result_text` roughly every 5 seconds (poll
`GET /api/reviews/{id}` to watch progress), and the full line-by-line
transcript is written to `<SATURNUS_REVIEW_LOG_DIR>/<review_id>.log`. The
review detail (and the bot result message) includes the `log=` path.

Result messages sent to the reviewer contain a concise conclusion, not the raw
log: the tool is asked to end with a `REVIEW SUMMARY:` line and the message
shows that summary (falling back to a short excerpt). ANSI color codes are
stripped from `result_text` and messages; the transcript log keeps the raw
output.

Repeated submissions of the same PR reuse one review record (and thread):
an active one is reported as already in progress, a finished one is reset and
re-run with the same id. Reviewer identities are resolved to names via the
contact API (new submissions, result messages, and backfilled on startup), and
the result message includes the opencode `session=` id so the reviewer can
continue the session with `opencode run --session <id>`.

The default command template is:

```text
run "Review GitHub PR {{.Repo}}#{{.PRNumber}} ({{.PRURL}}). The repository is already checked out at {{.Worktree}}. Fetch and check out the PR head yourself, then review the changes. Focus on correctness, security and style; be concise with file:line references."
```

Placeholders available in the template: `Repo`, `PRNumber`, `PRURL`, `Title`,
`BaseBranch`, `Worktree` (the mapped checkout path), `Guide` (per-repo review
guidelines), and `ReviewID`. The default template passes
`--title "saturnus-review-<id>"` so the opencode session can be located and
surfaced after the run. Set `SATURNUS_REVIEW_TOOL` and
`SATURNUS_REVIEW_COMMAND_TEMPLATE` together to swap in another tool.

### Per-repo review guides

`SATURNUS_REVIEW_GUIDES` maps a repo to a guide file whose contents are injected
into the review prompt:

```sh
export SATURNUS_REVIEW_GUIDES='milvus-io/milvus=/etc/saturnus/guides/milvus.md'
```

```text
# /etc/saturnus/guides/milvus.md
Do NOT run compilation. This is a huge C++/Go repo; review the diff without building.
```

If a repo has no explicit guide, a guide file named `.saturnus-review.md` (or
`.saturnus/review.md`) inside the checkout root is picked up automatically.
Explicit config wins over the in-repo file.

Note: because the tool runs inside the mapped checkout, that path must be
writable by the service user (e.g. listed under `ReadWritePaths` in the systemd
unit) if the tool needs to fetch or create branches.

Review records are exposed under `GET /api/reviews`, `GET /api/reviews/{id}`,
and `POST /api/reviews`, and shown in the web UI under **PR Reviews**.

Bot helpers: `/sat reviews` lists recent review requests, `/sat review <id>`
shows one, `/sat repos` shows the configured repo whitelist, `/sat whoami`
prints the sender's own open_id, and `/sat whois <name|email|mobile>` resolves a
user to their open_id.

Note: `whois` calls the Lark contact API (`users/search` for names,
`users/batch_get_id` for email/mobile), so the app needs the corresponding
contact read scope (e.g. `contact:user.base:readonly`).

See `docs/plan/pr-review.md` for the full design.

### Local Lark CLI Bridge

For local development, you can receive Lark messages through `lark-cli` instead
of exposing `/lark/events` with a public callback URL.

Start Saturnus in one terminal:

```sh
export LARK_APP_ID=cli_xxx
export LARK_APP_SECRET=xxx
export LARK_NOTIFY_CHAT_ID=oc_xxx
make run-server
```

Start the bridge in another terminal:

```sh
make lark-bridge
```

The bridge runs:

```text
lark-cli event consume im.message.receive_v1 --as bot
```

It converts each consumed message into the same `/lark/events` shape the server
already handles. This mode does not need ngrok or a public callback URL, but it
does require `lark-cli` to be configured with the app and bot scopes.

Useful bridge environment variables:

```sh
export SATURNUS_SERVER=http://localhost:8787
export SATURNUS_LARK_CLI=lark-cli
export SATURNUS_LARK_EVENT_KEY=im.message.receive_v1
export SATURNUS_LARK_AS=bot
```

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
