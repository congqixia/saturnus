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

- `/sat help` — show all available commands
- `/sat sessions`
- `/sat pending`
- `/sat session <session_id>`
- `/sat approve <request_id>`
- `/sat deny <request_id> <reason>`
- `/sat autopass on <session_id>`
- `/sat autopass off <session_id>`
- `/sat reviews`
- `/sat reviews all` / `/sat reviews archived` — include archived reviews, or list only archived
- `/sat review <pr-url>` (also works as natural language: any message containing a GitHub PR URL)
- `/sat review <pr-url> @user` — start the review but attribute the requester to the @mentioned user (e.g. when the request came to you by DM instead of the bot). `@all` and the bot itself are ignored; for chats where the user can't be @mentioned (e.g. a p2p chat with the bot) pass the open_id instead: `/sat review <pr-url> ou_xxx`
- `/sat review <review_id>`
- `/sat retry <review_id>` — retry a failed review, or re-run a successful one after its PR head commit moved
- `/sat reset <review_id>` — reset the tool session and queue a fresh review context, even if the PR head is unchanged
- `/sat post <review_id>` — publish a review's findings as GitHub inline comments (plus the verdict review); for reviews whose findings were never posted (feature off at the time, or a posting failure)
- `/sat refresh <review_id|all>` — re-sync PR/task state: archive merged/closed PRs (and complete their task), restore reopened PRs, sync new reviewer comments to the task
- `/sat act <review_id> <instruction>` — resume the review's tool session to act on the PR (e.g. post inline comments for selected issues, work out concrete code for an issue); the agent output is sent back to the chat
- `/sat threads <review_id>` — list every chat thread a review was requested in
- `/sat describe review <review_id>` — show full details of one review (as a card), including the run history
- `/sat repos` — show the configured repo whitelist
- `/sat prs` — list open PRs authored by the current `gh` user in the whitelisted repos, showing each PR's base branch
- `/sat groups` — list groups the bot has joined and their modes
- `/sat group [chat_id]` — show a group's mode (defaults to the current group)
- `/sat group mode <flags> [chat_id]` — set a group's mode flags (review, report, or report|review); omit `chat_id` when running inside the group
- `/sat whoami` — show the sender's own open_id
- `/sat whois <name|email|mobile>` — resolve a user to open_id via the contact API
- `/sat reply <review_id> <message>` — reviewer replies to the requester via DM
- `/sat requester <review_id> @user|ou_xxx` — update an existing review's requester (mention the user, or pass an open_id); archived reviews are rejected
- `/sat tasks` — list feature tasks (archived tasks are hidden)
- `/sat tasks all|archived` — list tasks including archived, or only archived
- `/sat task <task_id>` — show one task: issue, design docs, and per-PR progress
- `/sat task create <issue-url>` — create a task from a GitHub issue
- `/sat task add-doc <task_id> <owner/repo:path>` — attach a local design doc (a repo-relative path inside the whitelisted checkout) to a task
- `/sat task rm-doc <task_id> <doc_id>` — detach a design doc
- `/sat task add-pr <task_id> <pr-url>` — attach a PR and snapshot its review/Feishu-task state
- `/sat task rm-pr <task_id> <pr_id>` — detach a PR
- `/sat task refresh <task_id|all>` — re-sync issue/PR/review/Feishu-task state
- `/sat task close <task_id>` — archive a task

## PR Review

The server can accept PR review requests from the Lark bot conversation, record
the review request together with its thread context, optionally create a Feishu
task, and run a local tool (default `opencode`) to review the PR inside a mapped
local checkout. The result is sent to a fixed reviewer.

Enable it with:

```sh
export SATURNUS_REVIEW_ENABLED=1
export SATURNUS_REVIEW_ALLOWED_USERS=ou_xxx,ou_yyy     # senders who may request reviews
export SATURNUS_REVIEW_REVIEWER_OPEN_ID=ou_reviewer    # who receives the result (or REVIEWER_CHAT_ID)
export SATURNUS_REVIEW_REPOS_FILE=/etc/saturnus/repos.json  # create the mapping file below
export SATURNUS_REVIEW_CREATE_TASK=1                   # optional: create a Feishu task
export SATURNUS_REVIEW_REPLY_IN_THREAD=1               # optional: also reply in the original thread
make run-server
```

Create the mapping file before starting the server. It is a JSON object mapping
GitHub repo names to local checkout paths (see [repos.example.json](repos.example.json)):

```json
{
  "milvus-io/milvus": "/srv/checkouts/milvus",
  "milvus-io/milvus-storage": "/srv/checkouts/milvus-storage",
  "congqixia/saturnus": "../checkouts/saturnus"
}
```

Relative checkout paths are resolved against the mapping file's directory, not
the process working directory. Paths can contain commas, equals signs and spaces;
shell variables and `~` are not expanded. Repo names must have the form
`owner/repo`, paths must be non-empty strings, and duplicate repo keys are rejected.
An unreadable or invalid file prevents startup, even if review and Tasks are disabled.
The file is loaded once at startup; restart the corresponding process after editing it.

The legacy `SATURNUS_REVIEW_REPOS='owner/repo=/local/path,...'` format remains
supported, including its existing path and parsing behavior. Choose one source:
setting both `SATURNUS_REVIEW_REPOS` and `SATURNUS_REVIEW_REPOS_FILE` is an error.
When migrating, unset the old variable; the sources are never merged and a file
error never falls back to inline configuration. Other review environment variables,
including `SATURNUS_REVIEW_GUIDES`, keep their existing behavior.

The server mapping is shared with Tasks and its local review executor. Remote
workers register their own host-local mappings; the server file does not restrict
which repos remote workers can serve. A server may use an empty mapping (`{}`) or
no mapping when all reviews run remotely; workers must configure at least one repo.

Each mapped path must be a git checkout when a job runs. The server does not
fetch or check out anything itself: it runs the review tool directly inside the
mapped checkout and lets the tool fetch and check out the PR head on its own.

Environment variables:

| variable | default | note |
|---|---|---|
| `SATURNUS_REVIEW_ENABLED` | 0 | master switch; set to `1` to enable |
| `SATURNUS_COMMAND_ALLOWED_USERS` | - | who may run `/sat` commands; `*` = everyone, empty = deny all |
| `SATURNUS_REVIEW_ALLOWED_USERS` | - | comma-separated open_ids; `*` = allow everyone; empty = reject all |
| `SATURNUS_REVIEW_REPOS` | - | whitelist `owner/repo=/local/path,...` |
| `SATURNUS_REVIEW_REPOS_FILE` | - | JSON repo-to-checkout mapping file; mutually exclusive with `SATURNUS_REVIEW_REPOS` |
| `SATURNUS_REVIEW_REVIEWER_OPEN_ID` | - | DM recipient for results |
| `SATURNUS_REVIEW_REVIEWER_CHAT_ID` | - | chat recipient fallback |
| `SATURNUS_REVIEW_REPLY_IN_THREAD` | 0 | also reply into the original thread |
| `SATURNUS_REVIEW_TOOL` | opencode | review executable |
| `SATURNUS_REVIEW_COMMAND_TEMPLATE` | see below | `text/template` for command args |
| `SATURNUS_REVIEW_MAX_CONCURRENT` | 1 | worker count |
| `SATURNUS_REVIEW_LOCAL_FALLBACK` | 1 | set to `0` to keep reviews pending when remote workers are unavailable; repo mappings remain available to Tasks |
| `SATURNUS_REVIEW_TIMEOUT` | 15m | per-review timeout |
| `SATURNUS_REVIEW_CREATE_TASK` | 0 | create a Feishu task assigned to reviewer + requester |
| `SATURNUS_REVIEW_TASK_DUE_HOURS` | 24 | task due offset in hours |
| `SATURNUS_REVIEW_TASK_GROUP` | - | task group (任务分组) name tasks are filed under; Saturnus looks it up and creates it if missing (needs `task:tasklist:read/write`) |
| `SATURNUS_REVIEW_TASK_GROUP_ID` | - | pin an existing task group by guid, bypassing the name lookup/create |
| `SATURNUS_REVIEW_LOG_DIR` | `<tmp>/saturnus-review-logs` | per-review transcript logs |
| `SATURNUS_REVIEW_GUIDES` | - | per-repo review guide files: `owner/repo=/path/to/guide.txt,...` |
| `SATURNUS_REVIEW_RESULT_API_URL` | - | callback the review tool POSTs the structured result to (see below); may contain `{review_id}`, e.g. `http://127.0.0.1:8787/api/reviews/{review_id}/result` |
| `SATURNUS_REVIEW_RESULT_TOKEN` | - | optional bearer token the result endpoint requires; echoed into the tool prompt so the POST can authenticate |
| `SATURNUS_REVIEW_POST_INLINE_COMMENTS` | 0 | post the review findings as GitHub inline comments + a formal PR review (event mapped from the verdict) once the review succeeds; requires `gh` |
| `SATURNUS_REVIEW_POST_INLINE_MIN_SEVERITY` | P2 | lowest finding severity posted as an inline comment: `P2` = all, `P1` = only P0/P1 |
| `SATURNUS_REVIEW_NOTIFY_REQUESTER` | 0 | DM the requester a short result once the review succeeds: verdict, finding counts (P0/P1/P2), and how many inline comments were posted to the PR |

### Remote review workers

Reviews can also be executed on **remote hosts** that register with the server as
review workers (`cmd/saturnus-worker`). The server keeps all control-plane
responsibilities (scheduling, state, notifications, GitHub comment posting,
Feishu task sync); each worker owns its own repo checkouts and runs the review
tool locally. The worker keeps a single outbound WebSocket control channel to
the server, so no inbound ports are needed on the worker host.

The scheduler prefers a connected remote worker whose checkout serves the PR's repo,
routing retries and reviewer reactions back to the same host so the tool session
can be resumed. When no worker can serve a repo, the server falls back to its
own local executor (using the server's repo mapping), so existing
single-host deployments keep working unchanged.

When eligible connected workers are busy or backing off after a failed dispatch,
the review stays pending. Failed dispatches use bounded backoff instead of
immediately creating another run. Set `SATURNUS_REVIEW_LOCAL_FALLBACK=0` to
disable execution on the server while keeping its checkout mappings for Tasks.

Server side:

```sh
export SATURNUS_WORKER_REGISTER_KEY=secret     # required to accept workers
make run-server
```

Worker side (on each host that should provide review service):

```sh
export SATURNUS_SERVER=http://saturnus:8787     # server URL (worker must reach it)
export SATURNUS_WORKER_REGISTER_KEY=secret      # must match the server
export SATURNUS_REVIEW_REPOS_FILE=/etc/saturnus/worker-repos.json
export SATURNUS_WORKER_MAX_CONCURRENT=2
go run ./cmd/saturnus-worker                     # or make build-worker
```

Worker environment variables:

| variable | default | note |
|---|---|---|
| `SATURNUS_SERVER` | `http://localhost:8787` | server URL; the worker connects outbound (WS) |
| `SATURNUS_WORKER_ID` | hostname-derived | stable worker id |
| `SATURNUS_WORKER_REGISTER_KEY` | - | shared key, must match the server's |
| `SATURNUS_REVIEW_REPOS` | - | worker-local whitelist `owner/repo=/local/path,...` |
| `SATURNUS_REVIEW_REPOS_FILE` | - | worker-local JSON repo-to-checkout mapping file; mutually exclusive with `SATURNUS_REVIEW_REPOS` |
| `SATURNUS_REVIEW_TOOL` | opencode | review executable on the worker host |
| `SATURNUS_REVIEW_GH` | 1 | worker has `gh` available (used to resolve PR head/title) |
| `SATURNUS_WORKER_MAX_CONCURRENT` | 1 | concurrent reviews per worker |
| `SATURNUS_REVIEW_LOG_DIR` | `<tmp>/saturnus-review-logs` | per-review transcript logs on the worker |

Workers also accept `-repos-file /path/to/repos.json` and the legacy `-repos`
option. Each command-line option overrides its corresponding environment variable;
the resulting inline and file settings remain mutually exclusive. To switch
sources when an environment default is set, unset it or clear it explicitly,
for example `saturnus-worker -repos= -repos-file /etc/saturnus/worker-repos.json`.
Create each worker's file on that host; checkout paths refer to that host's filesystem.

Both ends of the WebSocket channel accept JSON messages up to 8 MiB, including
escaping and envelope overhead for the bounded tool transcript. Upgrade both the
server and workers to apply this limit.

`GET /api/workers` lists registered workers. Worker status is derived from
heartbeats: a worker whose connection has been silent longer than
`SATURNUS_WORKER_ONLINE_WINDOW` (default `90s`) is considered offline and its
in-flight reviews are re-queued to other workers.

### Worker GitHub version and credential expiry

With `SATURNUS_REVIEW_GH=1` (the default), the worker runs `gh --version` before
registering. A missing executable or unrecognized version output prevents startup.
The detected version and authentication metadata are included in
`capabilities.github`, available through `GET /api/workers`. PR metadata continues
to use the existing `gh pr view` command; this feature does not change that path.

Example of the decoded `capabilities.github` object:

```json
{
  "version": "2.86.0",
  "host": "github.com",
  "checked_at": "2026-10-27T08:00:00Z",
  "auth_status": "valid",
  "expiry_status": "known",
  "expiry_source": "github_response_header",
  "token_expires_at": "2026-11-01T02:00:00Z"
}
```

For PAT authentication, the worker uses an authenticated
`gh api --hostname github.com --include --silent user` request and reads the
`GitHub-Authentication-Token-Expiration` response header when GitHub supplies it.
For newer automatically refreshed OAuth credentials, `gh auth status --active
--hostname github.com --json hosts` can provide `tokenExpiresAt`, `refreshable`
and `refreshTokenExpiresAt`. The worker never requests `--show-token`, reads
credential storage, or reports token values to the server.

Expiry is not available for every authentication method. A missing header is
reported as `expiry_status=unknown`, not as proof that the token never expires.
Failed probes report `check_failed`; they do not prevent registration. Workers
recheck once an hour and publish new metadata via their existing authenticated
heartbeat. A failed refresh probe retains a previously known expiry so it cannot
silence an upcoming reminder. Token rotation is detected on the next successful
probe; restarting the worker probes immediately. `SATURNUS_REVIEW_GH=0` skips
all GitHub checks, and older workers without metadata remain supported.

The server checks stored expirations at startup, after registration/metadata
changes, and hourly. When less than seven days remain (including already expired
credentials), it notifies `SATURNUS_REVIEW_REVIEWER_OPEN_ID`, falling back to
`SATURNUS_REVIEW_REVIEWER_CHAT_ID` when no reviewer open_id is configured. Lark
app credentials must be configured. Without a recipient or enabled Lark client,
the condition is logged and is not marked delivered.

For the same worker, expiry and warning phase, successful reminders are limited
to once per 24 hours, with deduplication persisted in SQLite across server
restarts. Crossing into expired status sends a separate immediate reminder;
failed sends remain retryable. For credentials that `gh` refreshes automatically,
the reminder uses the **refresh token** expiry instead of treating the normal
short access-token lifetime as an outage. Unknown expiry cannot trigger a timed
reminder. Update both server and worker to enable the full feature.

While a review is running, the tool output is streamed: the accumulated text is
flushed into `result_text` roughly every 5 seconds (poll
`GET /api/reviews/{id}` to watch progress), and the full line-by-line
transcript is written to `<SATURNUS_REVIEW_LOG_DIR>/<review_id>.log`. The
review detail (and the bot result message) includes the `log=` path.

Server and worker process logs include `review_task` lifecycle events with
`review_id`, `worker_id`, `kind`, `repo`, `pr_number` and `status`; review runs
also carry `run_id`, and reviewer actions carry `reaction_id`. For example:

```text
review_task event="dispatch" component="server" kind="review" review_id="rvw_..." run_id="run_..." worker_id="wrk-host-a" repo="owner/repo" status="reviewing" pr_number=1
review_task event="started" component="worker" kind="review" review_id="rvw_..." run_id="run_..." worker_id="wrk-host-a" repo="owner/repo" status="running" pr_number=1
review_task event="completed" component="server" kind="review" review_id="rvw_..." run_id="run_..." worker_id="wrk-host-a" repo="owner/repo" status="succeeded" pr_number=1
```

The server logs `dispatch` before sending a job and `dispatched` after a
successful send. New workers report `started` back over WebSocket, so the server
can distinguish sending a task from the worker actually starting it. Workers log
`tool_started` with the executable and transcript `log_path`, then `completed`
and either `completion_reported` or `completion_report_failed`. Dispatch failures,
busy rejections, cancellation requests and requeues are also logged. Local
execution uses `worker_id="local"`. These process events do not include prompts,
instructions, callback credentials or tool output. Older workers remain supported
but do not send the new start event.

Filter both hosts' logs by `review_id="rvw_..."`; use `run_id` to distinguish
retries and `reaction_id` for reviewer actions. `/sat describe review <id>` and
`/sat review <id>` show the worker and registered hostname; `/sat reviews` shows
a Worker column. Describe's run and reaction histories list each execution's
worker. Local execution is labeled `server (local)`. A pending retry is labeled
`unassigned (previous: <worker>)` rather than implying it is already running on
the previous host.

Result messages sent to the reviewer contain a concise conclusion, not the raw
log. When `SATURNUS_REVIEW_RESULT_API_URL` is set, the review prompt asks the
tool to POST a machine-readable result to that endpoint (with the `{review_id}`
placeholder substituted and the token in `Authorization: Bearer <token>`) in
addition to printing the text `REVIEW SUMMARY:` block. The POST body is:

```json
{
  "verdict": "lgtm" | "request_change" | "nit_comments",
  "summary": "2-3 sentence summary of the PR and the review",
  "findings": [
    {
      "id": "fnd_... (prior finding id, only for status updates)",
      "status": "open" | "addressed" | "commented",
      "severity": "P0" | "P1" | "P2",
      "path": "repo-relative file path",
      "line": 1,
      "title": "short title",
      "message": "why it matters",
      "suggestion": "how to fix (optional)"
    }
  ]
}
```

`verdict` is the PR opinion: `lgtm` (approve as-is), `request_change` (one or
more blocking issues), `nit_comments` (no blockers, but P2 comments).
`findings` grade each concrete issue: `P0` = must fix before merge
(correctness / security / data loss), `P1` = should fix before merge
(significant bug or design flaw), `P2` = nice-to-have or nit. The server
validates the submission (`POST /api/reviews/{review_id}/result`, 401 without
the token, 409 when the review is not running or the callback belongs to an older
run) and atomically persists the verdict, summary and findings. Generated callback
URLs include a `run_id` query parameter; preserve the complete URL when POSTing.
Legacy callbacks without that parameter remain accepted. With a configured result
URL, a zero process exit without this run's structured result is recorded as
`failed`, with permission diagnostics when present. The tool must finish the POST
and verify success before exiting. Existing `succeeded` reviews missing a required
verdict can be rerun with `/sat retry`, even if the PR head has not changed.

The result message, `/sat describe review` card and web UI render the verdict and
findings directly. Historical findings stay visible when the current run has no
verdict. With no configured result URL, text-only reviews remain supported through
the `REVIEW SUMMARY:` block (`PR summary` / `Issues` / `Review suggestions`). ANSI color codes are stripped
from `result_text` and messages; the transcript log keeps the raw output.

### Finding status across review rounds

Findings are persistent per review, not replaced on retry. When a review is
retried/continued, the previous findings (with their ids, severity, location,
status and message) are injected into the tool prompt; the tool references each
one by id in the result and reports a new status instead of re-creating it:

- `open` — still an issue,
- `addressed` — the new PR head fixes it,
- `commented` — acknowledged, no code change needed.

New findings are submitted without an `id` (they default to `open`). On the
next round a finding can also be resolved on GitHub (see below). The status is
shown in `/sat describe review` and the web UI.

### Posting findings to GitHub

When `SATURNUS_REVIEW_POST_INLINE_COMMENTS=1` and a review finishes with a
structured verdict, the server publishes the result back on the PR via `gh`
(needs `gh` authenticated with repo scope, same host credentials as the
existing `gh` uses):

- Each finding with a concrete `path` + `line` and severity at or above
  `SATURNUS_REVIEW_POST_INLINE_MIN_SEVERITY` is posted as an inline comment on
  the PR head (`POST /repos/{owner}/{repo}/pulls/{n}/comments`). A finding
  whose line is not part of the PR diff is skipped with a logged error instead
  of failing the review. The returned GitHub comment id is stored on the
  finding (`github_comment_id`) so a later round can resolve it.
- A formal PR review is submitted whose event maps from the verdict —
  `lgtm` → `APPROVE`, `request_change` → `REQUEST_CHANGES`,
  `nit_comments` → `COMMENT` — with the summary as the review body.

On retry rounds, findings that already have a recorded comment are not
re-posted (no duplicates); instead, a finding the tool marked `addressed` has
its inline comment thread resolved on GitHub (via the GraphQL API,
best-effort) and its status becomes `resolved`.

Posting is best-effort: failures are logged and the local review still
succeeds. Since a re-run only happens after the PR head moves, comments are not
duplicated by retries.

The publish timestamp is stored (`comments_posted_at`, shown in `/sat review
<id>`, `/sat describe review`, and the web UI). Findings that were never posted
— e.g. reviews that finished before this feature was enabled, or a posting
failure — can be published retroactively with `/sat post <review_id>` (or
`POST /api/reviews/{review_id}/post-comments`). Re-triggering a review whose
findings were already posted is refused, so comments are never duplicated.

### Notifying the requester

By default only the reviewer receives the full result. With
`SATURNUS_REVIEW_NOTIFY_REQUESTER=1` the requester is additionally DM'd a short
summary once a review succeeds with a structured verdict:

```
PR 评审完成：owner/repo#42
https://github.com/owner/repo/pull/42
Verdict: Request change
Findings: 3 项 (P0: 1, P1: 1, P2: 1)
已发布 2 条 inline comment 到 PR
review: rvw_xxx
```

The count of posted comments reflects findings whose inline comment was
recorded (`github_comment_id`). The full detail still goes to the reviewer.

Repeated submissions of the same PR reuse one review record (and thread):
an active one is reported as already in progress. Each run records the PR head
commit (`head_commit`) it reviewed; a finished review is only reset and re-run
when its head commit moved, otherwise the request is reported as already up to
date (`ErrReviewUnchanged`). `/sat retry <review_id>` forces another run: failed
reviews always retry, successful ones only when the head changed. Retries keep
the previous tool session (`session_id`), so the run resumes the same review
conversation via `opencode run --session <id> --title ...`.

### Reset a review's tool context

Use `/sat reset <review_id>` when the saved tool session is bound to an expired
tool-account token or otherwise needs to be replaced. The command clears the
current `session_id` and queues a fresh run, including when the PR head has not
changed. It uses the same reviewer authorization as `/sat act` and rejects
archived reviews or reviews with pending/running review jobs or reactions.

Reset preserves the review ID, PR, requester, linked threads, prior findings,
run history and reaction history. Current verdict/output/publish markers are
cleared as for a normal re-run. The new session starts with a fresh conversation;
existing structured findings are still supplied for tracking across review rounds.
Old tool sessions and historical run session IDs are retained rather than deleted.

Each reset persists a new `context_id` in SQLite and isolates discovery with a
title such as `saturnus-review-<review_id>-<context_id>`. A reset context never
falls back to a session from another title or checkout, including when the new
tool run fails before creating a session. Subsequent ordinary retries and
reactions resume the new session. `/sat describe review` and the review detail
API (`context_id`) expose the active context; process logs record `context_reset`.

Update both server and worker. New workers register `context_reset=true` in
capabilities; the scheduler will not dispatch a reset context to an older worker.
If no compatible worker or local executor is available, the reset review remains
pending. Custom tool templates should use the session id conditionally and expose
the title through `--title` (or `--title=...`); the renderer substitutes the context
title there. `{{.SessionTitle}}` is also available for custom templates.

Reset changes the saved conversation, not the tool's authentication settings.
Configure valid credentials for the worker's current tool account before retrying.

### Reply-triggered re-review

In a group, replying inside the thread of a review-triggering message while
@mentioning the bot re-checks whether the PR head moved:

- If the head changed (or the review has no recorded baseline, e.g. a failed
  run), the review is re-queued, the requester gets an in-thread reply
  ("重新评审已开始") and the reviewer is notified about the trigger.
- If the head is unchanged, the bot replies in-thread that the review is
  already current (no re-run, no reviewer ping).

Explicit `/sat retry`, `/sat review`, or replies that contain a PR URL take
the normal paths instead. This requires the reply's thread context, which the
bridge now forwards (`root_id`/`thread_id`/`reply_to`). Reviewer identities
are resolved to names via the contact API (new submissions, result messages,
and backfilled on startup), and the result message includes the opencode
`session=` id so the reviewer can continue the session with
`opencode run --session <id>`.

The requester identity is stable across re-runs: re-submitting the same PR or
`/sat retry` (e.g. the reviewer continuing the review) never reassigns who the
requester is, so requester replies and task notifications keep reaching the
original requester. Only when no real requester is recorded (empty, or the
HTTP-API `"api"` sentinel) is the new sender adopted. The chat/message/thread
context still updates per request, so the review thread records everyone who
keeps participating.

A request can also be attributed to a different user than the command sender:
`/sat review <pr-url> @user` records the @mentioned user as the requester, so
they — not the admin who typed the command — receive the result notification
and task assignment. This is useful when a review was requested in a DM to a
human rather than to the bot. The requester name is taken from the mention
(falling back to the contact API), and access control stays on the actual
sender.

Note: attribution only happens on the explicit `/sat review <pr-url> @user`
command. Natural-language submissions (a bare PR URL pasted in a group) always
keep the message sender as the requester — in a group the @mentions next to a
PR link are usually the reviewer being asked to approve, not the requester, so
they must not be reinterpreted.

To reassign the requester of an existing review (e.g. after a mistake), use
`/sat requester <review_id> @user` (or pass the open_id directly). This updates
the stored requester and its display name; archived reviews are rejected.

Every thread a review is requested in is recorded in a `review_thread_links`
table (deduped per review+thread), so a review re-requested or retried from
different chats keeps its full thread history. `/sat threads <review_id>` lists
all of them (chat, topic, participant count, last activity) as a card, and the
history is also included in `GET /api/reviews/{id}` (`threads` field) and the
`/sat describe review` card. Reviews recorded before thread linking fall back
to their current thread.

### Refresh and archive

`/sat refresh <review_id>` (or `/sat refresh all` to sweep every non-archived
review) re-syncs a review with its PR and Feishu task:

- If the PR was merged or closed, the review is set to status `archived` (hidden
  from default listings) and its Feishu task is marked completed.
- If a previously archived PR was reopened, the review is restored to
  `succeeded`.
- If the reviewer submitted new reviews/comments on the PR, every comment is
  durably stored (the `review_comments` table is the source of truth) and the
  task description is rebuilt as a bounded, rotating summary of the newest ones
  (tracked by `task_synced_at`, so each comment is synced once). When the
  description hits the 3000-char limit, the oldest entries rotate out of the
  description but stay in the store, so a full description never blocks sync.
  This requires `gh` and the `task:task` write scope.
- The PR's current head commit is recorded as `pr_head_commit` (separate from
  `head_commit`, the last reviewed head), so listings can flag stale results.

Listings (`/sat reviews`, `/sat review <id>`, `/sat describe review`) compare
the last reviewed head against the PR head recorded at refresh: when they
differ, the status is marked **OUTDATED** (e.g. `succeeded / OUTDATED`),
meaning the PR moved since the review ran. Re-running the review
(`/sat retry`, a re-submission, or the reply-trigger) refreshes both heads, so
the tag clears once the review is current again.

Default listings (`/sat reviews`, `GET /api/reviews`) exclude archived reviews;
use `/sat reviews all` / `/sat reviews archived` (or `?archived=1` on the API)
to include or isolate them. Archived reviews reject re-runs
(`ErrArchived`), and carry an `archived_at` timestamp shown in the describe
card.

### Reactions (`/sat act`)

After a review finishes, the reviewer can act on it right from the chat without
manually shelling into the tool session:

```text
/sat act <review_id> 用 inline comment 在 PR 中回复 issue 1,3
/sat act <review_id> 针对 issue 2, 给出具体代码
```

`/sat act` resumes the review's tool session (`opencode run --session <id>`,
same session captured for the review), passes the instruction as-is, and runs
it in the background inside the mapped checkout. The agent's output is streamed
into a persisted reaction record and delivered back to the chat where the
command was issued (falling back to the configured reviewer recipient). Only
the reviewer (or an allowlisted review user) may trigger a reaction, and only
on a finished review — a missing tool session is reported immediately.

Every reaction is stored in a `review_reactions` table (review_id, instruction,
status, session, output, error, timestamps), shown in `GET /api/reviews/{id}`
(`reactions` field) and in the `/sat describe review` card, mirroring how
`review_runs` keeps run history. Reactions are re-enqueued after a server
restart; interrupted `running` ones are marked `failed`. The tool invocation
reuses the same `Run` helper as reviews, so ANSI stripping, log transcripts
(`<SATURNUS_REVIEW_LOG_DIR>/<reaction_id>.log`) and the per-job timeout apply
identically.

Every execution is also appended to a `review_runs` table (one row per run,
with `seq`, status, `head_commit`, retry reason, session and result). This
history survives later retries that overwrite the `review_requests` row, and is
exposed in `GET /api/reviews/{id}` and in the `/sat describe review` card.

When task creation is enabled, the requester is DM'd the generated Feishu task
with its clickable task link (`task_url`, an applink from the task API) so they
can open the task directly and know when to expect the result. Only one task is
created per PR: re-running an already-reviewed PR reuses the existing
`task_id`/`task_url` instead of creating a duplicate.

The default command template is:

```text
run "Review GitHub PR {{.Repo}}#{{.PRNumber}} ({{.PRURL}}). The repository is already checked out at {{.Worktree}}. Fetch and check out the PR head yourself, then review the changes. Focus on correctness, security and style; be concise with file:line references. Finish with a structured review. The very last block must be exactly:

REVIEW SUMMARY:
PR summary: <what the PR does in 2-3 sentences>
Issues: <each concrete problem with file:line references, one per line, or None>
Review suggestions: <verdict and next steps: LGTM, or the specific changes required, or a design/refactor suggestion>"
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

### Command access control

`/sat` commands require the sender to be listed in
`SATURNUS_COMMAND_ALLOWED_USERS` (or `*` for everyone; empty denies all).
Users outside that list can still trigger reviews by sending a GitHub PR URL in
plain chat, which is handled without the command layer. Any other message from
them gets a plain-text usage hint (no `/sat` syntax).

Note: `whois` calls the Lark contact API (`users/search` for names,
`users/batch_get_id` for email/mobile), and requester display names are
resolved via `users/:user_id`, so the app needs the corresponding
contact read scope. If the API answers with `code: 0` but only identity
fields (`open_id`/`union_id`) and no `name`, the app is missing the
`contact:user.base:readonly` scope (获取用户基本信息) — grant it and
re-publish the app. Name-lookup failures are logged server-side and never
block review processing (they fall back to showing the open_id, and are
backed off for 15 minutes to avoid hammering the API).

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
export SATURNUS_LARK_POST_TIMEOUT=5m   # max time to wait for the server's /lark/events response
```

Some `/sat` commands (e.g. `/sat refresh all`) run long enough that the sender
would otherwise wait without knowing whether the server received the command.
The bot therefore sends an ACK text message as soon as a `/sat` command is
received ("Command received, processing: ..."), then rewrites that same message
in place with the result once the command finishes (Feishu lets apps edit their
own text messages). Card replies are delivered as a separate card message and
the ACK is updated to a short "Done: ..." summary. This uses
`PUT /im/v1/messages/:message_id`, so the app needs the `im:message` /
`im:message:send_as_bot` scope (already implied by sending messages).

### Group mention gating

In group chats the bot only responds when it is **directly @mentioned** by its
own `open_id`. `@all` and mentions of other users do **not** trigger it;
direct (p2p) messages are always handled. The bot's own `open_id` is resolved
automatically via `bot/v3/info` at startup, or set explicitly:

```sh
export SATURNUS_LARK_BOT_OPEN_ID=ou_xxx
```

If the open_id cannot be resolved, group messages are ignored (with a warning
log) and only p2p messages are answered. The mention metadata is carried by
the bridge: `im.message.receive_v1` events expose a `mentions` list whose
`id` is each mentioned user's `open_id` (the special value `all` for
`@everyone`), and the server compares those ids against the bot's open_id.

### Per-group modes

The bot can behave differently in each group it has joined. The mode is a set
of `|`-joined capability flags (set with `/sat group mode`, listed with
`/sat groups`):

- **review** (default): the bot answers direct mentions, runs `/sat` commands
  and can start PR reviews in the group.
- **report**: the group is a quiet report channel — the bot stays silent to
  ordinary messages, and the group is only used to publish periodic reports
  (message sending from the server is unaffected).
- **report|review**: quiet for ordinary messages and non-review `/sat`
  commands, but review requests (a PR URL or `/sat review`/`retry`/`refresh`/
  `act`/`threads`/`describe`/`reply` with a bot mention) are still acted on.

The mode is persisted in the SQLite store. To set it, either run the command
**inside** the group (the bot senses the current chat) or pass the `chat_id`
explicitly from anywhere. Flags are normalized (order and separators do not
matter):

```text
/sat group mode report           # inside the target group (mention the bot)
/sat group mode report oc_xxx    # from any chat, using the group id
/sat group mode report|review    # quiet group that still runs reviews
/sat group mode review oc_xxx    # switch a group back to full interactive
```

`/sat groups` lists every group the bot has joined (via `im/v1/chats`) with its
mode, so you can pick the `chat_id` for groups you are not currently in. A
group in report mode ignores everything, including `/sat` commands sent inside
it, so toggle it back by id from another chat. The bot needs the `im:chat`
read scope to list and resolve chats.

## Tasks

The Tasks module tracks a feature/effort end to end by binding it to **one
GitHub issue**, the **local design docs** that define it, and the **PRs** that
implement it. For every linked PR it tracks progress independently: PR state
(open/closed/merged), review status, and the Feishu task created for the PR's
review. Everything is queryable from the bot (`/sat task ...`) and the web UI
(Tasks panel).

Enable it with:

```sh
export SATURNUS_TASKS_ENABLED=1
export SATURNUS_TASKS_GH_CLI=gh            # optional; default gh
export SATURNUS_REVIEW_REPOS_FILE=/etc/saturnus/repos.json   # shared repo whitelist
make run-server
```

The repo whitelist is shared with the review module (`SATURNUS_REVIEW_REPOS_FILE`
or the legacy `SATURNUS_REVIEW_REPOS`):
only whitelisted repos can host issues, design docs and PRs.

Workflow:

```text
/sat task create https://github.com/acme/foo/issues/42   # bind the issue
/sat task add-doc <task_id> acme/foo:docs/plan/feature.md  # attach a design doc
/sat task add-pr <task_id> https://github.com/acme/foo/pull/5  # attach a PR
/sat task refresh <task_id>     # re-sync issue/PR/review/Feishu-task state
/sat task <task_id>             # show the full picture
```

Per-PR progress is tracked **standalone**: each `task_prs` row keeps its own
snapshot of PR state, review status, and Feishu task id/url. The snapshot is
copied from the linked `review_requests` row when a review exists (via
`review_id`), so it never depends on a live review lookup to render. Attaching a
PR never starts a review; use `/sat review <pr-url>` as usual, then refresh the
task to pick up the review status and its Feishu task.

Environment variables:

| variable | default | note |
|---|---|---|
| `SATURNUS_TASKS_ENABLED` | 0 | master switch; set to `1` to enable |
| `SATURNUS_TASKS_GH_CLI` | gh | GitHub CLI for issue/PR resolution |
| `SATURNUS_REVIEW_REPOS` | - | shared repo whitelist `owner/repo=/local/path,...` |
| `SATURNUS_REVIEW_REPOS_FILE` | - | shared JSON repo-to-checkout mapping file; mutually exclusive with `SATURNUS_REVIEW_REPOS` |

See `docs/plan/tasks.md` for the full design.

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

## Task background refresh

When `SATURNUS_TASKS_ENABLED=1`, the server immediately scans `open` and
`in_progress` tasks in the background, then waits five minutes after each scan.
It refreshes issues, discovers linked PRs, updates PR metadata, synchronizes
existing reviews when enabled, and snapshots their resulting state. Closed PRs
are checked too, so reopening can be detected. It never starts reviews or
archives feature tasks automatically.

| Variable | Default | Meaning |
| --- | --- | --- |
| `SATURNUS_TASKS_REFRESH_INTERVAL` | `5m` | Delay after a completed scan; `0` disables automatic refresh |
| `SATURNUS_TASKS_REFRESH_TIMEOUT` | `60s` | Per-task refresh timeout; must be positive |

Invalid durations fail server startup. Manual refresh retains its existing
non-archived scope, including `done` tasks. Automatic refresh runs independently
of Web polling and preserves successful snapshots when a remote query fails.
Task and PR details expose the latest successful refresh and stage errors.
With review/Lark enabled, existing review refresh effects also apply, including
Feishu task completion and incremental submitted-review descriptions. The
scheduler is cancelled and joined before SQLite closes during server shutdown.
See [Task Background Refresh](docs/plan/task-auto-refresh.md) for design details.
