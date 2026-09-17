# PR Review Extension Plan

## Goal

Extend Saturnus so that a Lark bot conversation can request a PR review. The
server records the review request together with the originating Lark thread
context, optionally creates a Feishu task to track it, and (optional) runs a
local tool such as `opencode` to review the PR and delivers the result to a
fixed reviewer.

## Capabilities

1. Accept PR review requests from bot conversations (explicit command or
   natural-language PR URL mention).
2. Persist review requests and their Lark thread context on the server.
3. (Optional) Create a Feishu task for each review request.
4. (Optional, template-driven) Run a local review tool (default `opencode`) in
   a fresh PR worktree and send the result to the configured reviewer.

Items 3 and 4 are behind config flags so the pipeline can be rolled out in
stages.

## Runtime Components

### Server

The server owns everything: intent parsing, persistence, Feishu task creation,
the local executor, and result delivery. No new binary is required; the worker
pool runs inside `cmd/saturnus-server`.

### Bot Conversation

Two ways to trigger a review:

- Explicit command: `/sat review <pr-url>`
- Natural language: any message containing a GitHub PR URL
  (`github.com/<owner>/<repo>/pull/<n>`), e.g. `please review PR
  https://github.com/foo/bar/pull/42`.

The webhook / bridge message context (chat_id, sender open_id, message_id,
root/thread id) is captured at ingest time so replies can be threaded.

### Review Worker

An in-process queue + bounded worker pool. A job is a review ID. Workers:
resolve PR metadata, run the configured tool in the mapped checkout, store the
result, and notify the reviewer. `pending` jobs are re-enqueued on restart;
orphaned `reviewing` jobs are marked `failed`.

## Data Model

New tables in `internal/store`:

### review_threads

Records the Lark conversation context for a review request.

| column | type | note |
|---|---|---|
| id | TEXT PK | `thr_...` |
| chat_id | TEXT | Lark chat |
| root_message_id | TEXT | thread/topic root message |
| topic | TEXT | topic title if any |
| participants | TEXT | JSON array of open_ids |
| last_message_at / created_at | TEXT | timestamps |

Keyed logically by `(chat_id, root_message_id)` so follow-ups reuse the same
thread row.

### review_requests

| column | type | note |
|---|---|---|
| id | TEXT PK | `rvw_...` |
| thread_id | TEXT FK | -> review_threads |
| status | TEXT | pending / reviewing / succeeded / failed / cancelled |
| pr_url | TEXT | canonical PR URL |
| repo | TEXT | `owner/repo` |
| pr_number | INTEGER | |
| title / base_branch | TEXT | filled from `gh pr view` when available |
| requester_open_id / requester_name | TEXT | who asked |
| chat_id / message_id | TEXT | where the result can be answered |
| tool | TEXT | tool used (default `opencode`) |
| task_id | TEXT | Feishu task GUID when created |
| task_url | TEXT | clickable Feishu task applink |
| result_text | TEXT | tool output |
| error | TEXT | failure detail |
| created_at / updated_at / completed_at | TEXT | |

## Flow

1. Message arrives at `/lark/events`. Context is extracted (chat_id, sender
   open_id, message_id, root id, text).
2. Parser matches a review intent. If the sender is not in the allowlist the
   request is rejected with a reply.
3. Server creates/updates a `review_threads` row, then a `review_requests` row
   with status `pending`.
4. Ack is sent back to the chat: `Review started rvw_xxx`.
5. If task creation is enabled, a Feishu task is created and `task_id` is
   stored.
6. The review ID is enqueued. A worker picks it up:
   - Resolve the local checkout from the repo whitelist; reject if not listed.
   - `gh pr view` (when `gh` exists) for title/base branch.
   - Run the tool via the command template directly inside the checkout. The
     tool is responsible for fetching/checking out the PR head itself.
   - Store result / error, mark `succeeded` or `failed`.
7. Result is delivered to the fixed reviewer (DM by open_id, or configured
   chat; optionally also replied into the original thread).

## Repo Whitelist

Review is only allowed for repos explicitly mapped to a local checkout.
`SATURNUS_REVIEW_REPOS=owner/repo=/local/path,owner2/repo2=/path2`. Nothing is
checked out by default: a repo outside the whitelist is rejected at submission
time (`repo is not in the review whitelist`) and checked again by the worker as
defense in depth. Each mapped path must be a git checkout. Saturnus never
fetches or creates worktrees itself; the review tool runs inside the checkout
and handles the PR head fetch/checkout on its own.

## Local Executor

Command is built from a template so other tools can be plugged in. The tool
runs with its working directory set to the mapped checkout (the `Worktree`
placeholder) and handles the PR head fetch/checkout itself; Saturnus does not
check out anything.

- `SATURNUS_REVIEW_TOOL` — executable, default `opencode`.
- `SATURNUS_REVIEW_COMMAND_TEMPLATE` — `text/template` over a struct with
  `Repo`, `PRNumber`, `PRURL`, `Title`, `BaseBranch`, `Worktree`, `Guide`.
  Default:
  `run "Review GitHub PR {{.Repo}}#{{.PRNumber}} ({{.PRURL}}). The repository
  is already checked out at {{.Worktree}}. Fetch and check out the PR head
  yourself, then review the changes. Focus on correctness, security and style;
  be concise with file:line references.{{if .Guide}} Review guidelines:
  {{.Guide | shellquote}}{{end}} Finish with a structured review. The very last
  block must be exactly:
  REVIEW SUMMARY:
  PR summary: <what the PR does in 2-3 sentences>
  Issues: <each concrete problem with file:line references, one per line, or None>
  Review suggestions: <verdict and next steps: LGTM, or the specific changes required, or a design/refactor suggestion>"`
  The three-section block is parsed from the tool output and rendered as the
  result message; if the tool omits the headers the raw summary is shown.

The `shellquote` template function escapes backslashes and double quotes so the
guide content survives the quoted-arg parsing.

### Per-repo review guides

`SATURNUS_REVIEW_GUIDES=owner/repo=/path/to/guide.txt,...` attaches a guide file
to a repo; its contents are injected into the prompt (used e.g. to tell the tool
not to compile on huge repos like milvus). If no explicit guide is configured, a
`.saturnus-review.md` (or `.saturnus/review.md`) file inside the checkout root
is discovered. Explicit config takes precedence over the in-repo file.

The subprocess runs with `Dir = Worktree`, a per-job timeout, and combined
output capture. Output is streamed for debugging: every ~5 seconds the
accumulated text is flushed into `review_requests.result_text` (so
`GET /api/reviews/{id}` shows progress), and the full transcript is written to
`<SATURNUS_REVIEW_LOG_DIR>/<review_id>.log`. On Linux the tool runs in its own
process group, so a timeout kills the whole tree including tool-spawned
children.

## Feishu Task

When `SATURNUS_REVIEW_CREATE_TASK=1`, the server calls
`POST /open-apis/task/v2/tasks` (summary = PR title, description = PR URL +
review ID, member = reviewer, due = now + configured hours). The returned
`guid` is stored as `task_id` (and the clickable `url` as `task_url`), and the
requester is DM'd the task link so they can open it directly. Requires
`task:task` write scope for the app.

## Config

| env | default | note |
|---|---|---|
| SATURNUS_REVIEW_ENABLED | 0 | master switch; `1` enables |
| SATURNUS_REVIEW_ALLOWED_USERS | - | comma-separated open_ids; `*` = allow everyone; empty = reject all |
| SATURNUS_REVIEW_REVIEWER_OPEN_ID | - | DM recipient |
| SATURNUS_REVIEW_REVIEWER_CHAT_ID | - | chat recipient fallback |
| SATURNUS_REVIEW_REPLY_IN_THREAD | 0 | also reply into original thread |
| SATURNUS_REVIEW_REPOS | - | whitelist `owner/repo=/local/path,...` |
| SATURNUS_REVIEW_TOOL | opencode | executable |
| SATURNUS_REVIEW_COMMAND_TEMPLATE | see above | command args template |
| SATURNUS_REVIEW_MAX_CONCURRENT | 1 | worker count |
| SATURNUS_REVIEW_TIMEOUT | 15m | per-review timeout |
| SATURNUS_REVIEW_CREATE_TASK | 0 | create Feishu task |
| SATURNUS_REVIEW_TASK_DUE_HOURS | 24 | task due offset |
| SATURNUS_REVIEW_LOG_DIR | `<tmp>/saturnus-review-logs` | per-review transcript logs |
| SATURNUS_REVIEW_GUIDES | - | per-repo guide files: `owner/repo=/path/guide.txt,...` |

## API Additions

- `GET /api/reviews?status=...`
- `GET /api/reviews/{id}`
- `POST /api/reviews` (JSON body, e.g. `{"pr_url": ...}`)

## Bot Commands

- `/sat review <pr-url>`
- `/sat reviews` — recent review requests
- `/sat review <id>` — detail of one review
- `/sat repos` — show the configured repo whitelist

## Web UI

A Reviews panel lists requests with status tags; detail shows result text.

## Security Notes

- Only allowlisted users may trigger reviews.
- Only whitelisted repos (mapped to a local checkout) can be reviewed; no
  checkout happens by default.
- PR URLs must be GitHub PR URLs.
- Saturnus never checks out PR branches itself; the tool runs in the existing
  checkout and handles the PR head fetch/checkout.
- No secrets are injected into the subprocess; git/gh use host credentials.
- Bounded concurrency and per-job timeout.

## Milestones

1. Store schema + review service + worker (items 1-2).
2. Feishu task creation (item 3).
3. Local executor + result delivery (item 4).
4. Web UI panel.
