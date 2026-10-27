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
5. Reviewer reactions: after a review finishes, the reviewer can resume the
   review's tool session from the chat (`/sat act <review_id> <instruction>`)
   so the agent takes an action (inline comments on selected issues, concrete
   code for an issue, ...) and the output is returned.

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
| session_id | TEXT | tool session captured after a run; reused to continue a retry |
| head_commit | TEXT | PR head commit (`headRefOid`) this run reviewed |
| retry_reason | TEXT | why the run was re-queued (shown in prompts/describe, cleared after the run) |
| result_text | TEXT | tool output |
| verdict | TEXT | structured PR opinion of the last run: `lgtm` / `request_change` / `nit_comments` (empty until the tool POSTs a result) |
| summary | TEXT | structured short overview of the review (empty until posted) |
| comments_posted_at | TEXT | when the findings were last published to the PR as inline comments; reset on re-run, and `POST .../post-comments` skips a review with this set |
| error | TEXT | failure detail |
| archived_at | TEXT | when the review was archived (PR merged/closed); hidden from default listings |
| refreshed_at | TEXT | last `/sat refresh` time |
| task_synced_at | TEXT | newest PR review `submitted_at` already synced to the task |
| created_at / updated_at / completed_at | TEXT | |

The synced PR review comments live in `review_comments` (the source of truth
for the review log): each row holds the review id, dedupe key (GitHub review id
or a content digest), author, state, body and `submitted_at`. The Feishu task
description is rebuilt from this table as a bounded, rotating summary of the
newest comments, so an over-limit description rotates the oldest entries out
without losing history.

Every execution is also appended to `review_runs` (one row per run, ordered by
`seq`), so the full history — including each run's status, head commit, retry
reason, session and result — is preserved even after later retries overwrite
the `review_requests` row:
| column | meaning |
| --- | --- |
| id / review_id / seq | run id, owning review, 1-based sequence |
| status | running / succeeded / failed / cancelled |
| head_commit | PR head commit this run reviewed |
| retry_reason | why this run happened (empty for the first run) |
| session_id | tool session captured for this run |
| result_text / error | this run's outcome |
| verdict / summary | this run's structured verdict/summary when submitted |
| started_at / completed_at / updated_at | timing |

`review_runs` is exposed in `GET /api/reviews/{id}` (`runs`) and in the
`/sat describe review` card.

### review_findings

Records the concrete P0-P2 issues the review tool POSTed for the current
result, so the verdict is actionable and machine-readable:

| column | meaning |
| --- | --- |
| id / review_id | `fnd_...`, owning review |
| run_id | the run that produced or last updated the finding |
| severity | P0 (must fix) / P1 (should fix) / P2 (nit) |
| path / line | repo-relative file and 1-based line (0 when not applicable) |
| title / message / suggestion | short title, why it matters, how to fix |
| status | open / addressed / commented / resolved |
| github_comment_id | GitHub inline-comment id posted for the finding (empty until posted) |
| created_at | timing |

Findings persist across review rounds: a retry/continue result references prior
findings by id and updates their status in place, new findings are appended.
They are exposed in `GET /api/reviews/{id}` (`findings`) and rendered in the
describe card and web UI.

### review_reactions

Records reviewer-triggered actions on a finished review, so every `/sat act`
instruction (and its outcome) is traceable:

| column | meaning |
| --- | --- |
| id / review_id | `rxn_...`, owning review |
| instruction | the reviewer's natural-language instruction |
| status | pending / running / succeeded / failed |
| chat_id / message_id | where the command was issued (result is delivered there) |
| sender_open_id / sender_name | who triggered the action |
| session_id | tool session resumed for this action |
| result_text / error | the agent's output / failure detail |
| created_at / updated_at / completed_at | timing |

Exposed in `GET /api/reviews/{id}` (`reactions`) and the `/sat describe
review` card.

### review_thread_links

Records every thread a review was requested in, so the review keeps its full
chat-thread history even after `review_requests.thread_id` moves to the latest
request's thread:

| column | meaning |
| --- | --- |
| review_id | owning review |
| thread_id | the thread used for that request |
| linked_at | when the thread was (last) used for the review |

Deduped by `(review_id, thread_id)`; on every submit/retry the resolved thread
is linked. `GET /api/reviews/{id}` returns `threads`, the `/sat threads
<review_id>` command lists them as a card, and the describe card shows them.
Reviews recorded before linking fall back to their current thread.

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
   - `gh pr view` (when `gh` exists) for title/base branch/head commit; the head
     commit is recorded as `head_commit` and used as the baseline for retries.
   - If the review was re-queued (retry/re-submit), the command template resumes
     the previous tool session (`--session <session_id>`) and the prompt notes
     the `retry_reason` so the tool continues the existing review conversation.
   - Run the tool via the command template directly inside the checkout. The
     tool is responsible for fetching/checking out the PR head itself.
   - Store result / error, mark `succeeded` or `failed`.
7. Result is delivered to the fixed reviewer (DM by open_id, or configured
   chat; optionally also replied into the original thread).

Re-submission and retry semantics:

- Submitting the same PR again while a review is active → already in progress.
- Submitting again after a review finished: a failed review always re-runs; a
  successful review only re-runs when `head_commit` moved (compared via
  `gh pr view --json headRefOid`), otherwise the request is refused as already
  up to date. The retry keeps `session_id` and sets `retry_reason`.
- `/sat retry <review_id>`: same policy, plus it works on a specific review id.
  The previous `session_id` is kept so the run appends to the same tool
  conversation.
- The requester identity is preserved across re-runs: re-submitting or retrying
  a review (e.g. the reviewer continuing the review) does not reassign
  `requester_open_id`/`requester_name`, so requester replies and task
  notifications still reach the original requester. Only when the recorded
  requester is a placeholder (empty, or the HTTP-API `"api"` sentinel) is the
  new sender adopted. The chat/message/thread context is still updated each
  request so the review thread records who keeps participating.

### Refresh and archive

`/sat refresh <review_id>` (or `/sat refresh all` to sweep every non-archived
review) re-syncs a review with its PR and Feishu task:

- `gh pr view` resolves the PR state. When it is `MERGED` or `CLOSED` the
  review is set to status `archived` (`archived_at` recorded) and, when a task
  exists, the task is marked completed. Archived reviews are hidden from
  default listings and reject re-runs (`ErrArchived`).
- A previously archived PR that was reopened restores the review to `succeeded`
  and clears `archived_at`.
- For an open PR, `gh api repos/<owner>/<repo>/pulls/<n>/reviews` returns the
  submitted reviews. Every review comment is persisted durably in the
  `review_comments` store table (the source of truth, deduped by review id), and
  the task description is rebuilt as a bounded, rotating summary of the newest
  comments (`[review] <user> (<state>) <submitted_at>\n<body>`, capped at 3000
  UTF-8 chars). When the description reaches the limit, the oldest entries
  rotate out of the description but stay in the store, so a full description
  never blocks sync; then `task_synced_at` advances so each comment is synced
  once. Requires `gh` and the `task:task` write scope.

Task writes follow the task v2 update contract: a nested `task` object plus an
`update_fields` list naming the changed fields, and completion is set via
`completed_at` (ms timestamp; `"0"` reopens) — not a boolean `completed` field.
API errors surface Feishu's `code`/`msg` in the log so a 400 (e.g. code
`1470400`) is diagnosable.

Listings: `ListReviews(status, includeArchived)` excludes archived by default;
`/sat reviews all` / `/sat reviews archived` and `GET /api/reviews?archived=1`
opt in.

### Reactions

`/sat act <review_id> <instruction>` records a reaction (status `pending`) and
enqueues it on the same worker pool. A worker resolves the review's checkout,
resumes the review's tool session (`opencode run --session <session_id>`; the
stored `session_id`, falling back to `captureSessionID`), and runs the tool
with the instruction passed as a single argv element (no shell quoting issues).
Output is streamed into `review_reactions.result_text` (~5s flush), the
transcript is written to `<LOG_DIR>/<reaction_id>.log`, and the outcome is
delivered to the chat where the command was issued (falling back to the
configured reviewer recipient). The instruction argv element keeps arbitrary
reviewer text (quotes, backslashes, whitespace) verbatim.

Validation at submission time: service enabled, sender is the reviewer
(`isReviewer`), instruction non-empty, review exists and is not still running,
and a tool session is resolvable. Pending reactions are re-enqueued on
restart; interrupted `running` reactions are marked `failed`.

## Repo Whitelist

Review is only allowed for repos explicitly mapped to a local checkout on the
server or served by a registered online worker. Configure each host using
`SATURNUS_REVIEW_REPOS_FILE=/path/to/repos.json`, a JSON object mapping
`owner/repo` to checkout path, or the legacy inline
`SATURNUS_REVIEW_REPOS=owner/repo=/local/path,owner2/repo2=/path2`. The sources
are mutually exclusive. File configuration is validated once before startup;
duplicate repo keys and invalid entries fail startup, and relative paths resolve
against the file's directory. No shell expansion is performed. Restart after
editing the file. See [the example](../../repos.example.json).

Nothing is checked out by default: an unserved repo is rejected at submission
time and checked again by the executor. The server's mapping is shared with
Tasks; remote workers keep their own host-local mappings. An empty server map
is allowed for remote-only review deployments; workers require a non-empty map.
Each mapped path must be a git checkout when a job runs. Saturnus never fetches
or creates worktrees itself; the review tool runs inside the checkout and handles
the PR head fetch/checkout on its own.

## Local Executor

Command is built from a template so other tools can be plugged in. The tool
runs with its working directory set to the mapped checkout (the `Worktree`
placeholder) and handles the PR head fetch/checkout itself; Saturnus does not
check out anything.

- `SATURNUS_REVIEW_TOOL` — executable, default `opencode`.
- `SATURNUS_REVIEW_COMMAND_TEMPLATE` — `text/template` over a struct with
  `Repo`, `PRNumber`, `PRURL`, `Title`, `BaseBranch`, `Worktree`, `Guide`,
  `ResultAPIURL`, `ResultToken`.
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

### Structured result (verdict + P0-P2 findings)

`SATURNUS_REVIEW_RESULT_API_URL` (default empty) turns the review result into a
machine-readable record. The URL may contain `{review_id}`; it is substituted
per review and injected into the prompt, together with the JSON body format and
(optionally) `SATURNUS_REVIEW_RESULT_TOKEN` as
`Authorization: Bearer <token>`. The tool is instructed to POST:

```json
{
  "verdict": "lgtm" | "request_change" | "nit_comments",
  "summary": "<2-3 sentence summary>",
  "findings": [
    {"id": "<prior finding id, for status updates>", "status": "open"|"addressed"|"commented", "severity": "P0"|"P1"|"P2", "path": "<file>", "line": <int>, "title": "<short>", "message": "<detail>", "suggestion": "<fix>"}
  ]
}
```

- `verdict` is the PR opinion: `lgtm` (approve as-is), `request_change` (one or
  more blocking P0/P1 issues), `nit_comments` (no blockers but P2 comments).
- `findings` grade each concrete issue: P0 = must fix (correctness/security/data
  loss), P1 = should fix (significant bug/design flaw), P2 = nice-to-have/nit.
  A finding carrying an `id` references a finding from a previous round and
  updates its status (`open` = still an issue, `addressed` = fixed in the new
  PR head, `commented` = acknowledged without a code change); a finding without
  an `id` is new and defaults to `open`. Findings are persisted (upserted), not
  replaced, so prior rounds' findings are carried into the next round's prompt.
- The endpoint `POST /api/reviews/{id}/result` validates the payload and stores
  `verdict`/`summary` on the review (and its current run) plus the findings.
  While the review is running submissions update the recorded result; a
  duplicate after completion (verdict already recorded) is rejected 409, and a
  wrong/missing token is rejected 401.

On retry/continue the prompt lists the previous findings (id, severity,
location, status, message) so the tool references them by id and reports the
new status instead of re-reporting them as new issues.

The result message, `/sat describe review` card and web UI render the verdict
and the P0-P2 findings directly; without a submitted result they fall back to
the text `REVIEW SUMMARY:` block. `finish` writes the review outcome with a
targeted UPDATE (only status/result/error/completed_at), and progress flushes
touch only `result_text`, so a POST arriving mid-run is never clobbered.

### Posting findings to the PR

`SATURNUS_REVIEW_POST_INLINE_COMMENTS=1` makes the server publish the
structured result back onto the GitHub PR using the same `gh` CLI as the rest
of the pipeline (`SATURNUS_REVIEW_TOOL` / `SATURNUS_TASKS_GH_CLI` host
credentials):

- Findings with a concrete `path` + `line` whose severity is at or above
  `SATURNUS_REVIEW_POST_INLINE_MIN_SEVERITY` are posted as standalone inline
  comments on the PR head via `POST /repos/{owner}/{repo}/pulls/{n}/comments`
  (`commit_id` = `head_commit`, `side` = `RIGHT`). Each comment is independent,
  so a finding whose line is not part of the diff (GitHub 422) is skipped with
  a logged error instead of failing the review. The response's comment `id` is
  stored on the finding (`github_comment_id`).
- A formal review is submitted via `POST /repos/{owner}/{repo}/pulls/{n}/reviews`
  with the event mapped from the verdict — `lgtm`→`APPROVE`,
  `request_change`→`REQUEST_CHANGES`, `nit_comments`→`COMMENT` — and the
  summary as body.

On later rounds findings that already have a `github_comment_id` are not
re-posted (no duplicates). Instead, a finding the tool marked `addressed` has
its comment thread resolved on GitHub via the GraphQL API
(`resolveReviewThread`, located through the comment `databaseId`) and its
status becomes `resolved`.

Posting happens in `process` after `finish` for `succeeded` reviews with a
recorded verdict, is best-effort (errors logged, review unaffected), and is not
duplicated across retries because a re-run requires the PR head to move.

With `SATURNUS_REVIEW_NOTIFY_REQUESTER=1` the requester is DM'd a short result
once a review succeeds with a verdict: the verdict, the finding count by
severity, and how many inline comments were already posted to the PR (from
`github_comment_id`). The full detail stays with the reviewer.

Older reviews whose findings were never posted (feature was disabled when they
ran, or a posting failure) can be published manually:

- `/sat post <review_id>` (reviewer command)
- `POST /api/reviews/{review_id}/post-comments`

`review_requests.comments_posted_at` records the last successful publish
(shown in describe/listings); a re-trigger of an already-posted review is
refused so comments are never duplicated, and the marker is reset when the
review is re-run so a fresh run publishes its new findings.

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
| SATURNUS_REVIEW_REPOS_FILE | - | JSON repo-to-checkout mapping file; mutually exclusive with SATURNUS_REVIEW_REPOS |
| SATURNUS_REVIEW_TOOL | opencode | executable |
| SATURNUS_REVIEW_COMMAND_TEMPLATE | see above | command args template |
| SATURNUS_REVIEW_MAX_CONCURRENT | 1 | worker count |
| SATURNUS_REVIEW_TIMEOUT | 15m | per-review timeout |
| SATURNUS_REVIEW_CREATE_TASK | 0 | create Feishu task |
| SATURNUS_REVIEW_TASK_DUE_HOURS | 24 | task due offset |
| SATURNUS_REVIEW_LOG_DIR | `<tmp>/saturnus-review-logs` | per-review transcript logs |
| SATURNUS_REVIEW_GUIDES | - | per-repo guide files: `owner/repo=/path/guide.txt,...` |
| SATURNUS_REVIEW_RESULT_API_URL | - | callback URL (may contain `{review_id}`) the tool POSTs the structured result to; empty disables the result POST |
| SATURNUS_REVIEW_RESULT_TOKEN | - | bearer token required by the result endpoint, echoed into the prompt |
| SATURNUS_REVIEW_POST_INLINE_COMMENTS | 0 | post findings as GitHub inline comments + a formal verdict review when a review succeeds; requires `gh` |
| SATURNUS_REVIEW_POST_INLINE_MIN_SEVERITY | P2 | lowest severity posted inline: `P2` = all, `P1` = only P0/P1 |
| SATURNUS_REVIEW_NOTIFY_REQUESTER | 0 | DM the requester a short result summary (verdict, finding counts, comments posted) when a review succeeds |

## API Additions

- `GET /api/reviews?status=...`
- `GET /api/reviews/{id}` (also returns `findings`)
- `POST /api/reviews` (JSON body, e.g. `{"pr_url": ...}`)
- `POST /api/reviews/{id}/result` (structured verdict/summary/findings, advertised in the tool prompt)
- `POST /api/reviews/{id}/post-comments` (publish a review's findings as GitHub inline comments + verdict review)

## Bot Commands

- `/sat review <pr-url>`
- `/sat reviews` — recent review requests (excludes archived)
- `/sat reviews all|archived` — include archived, or only archived
- `/sat review <id>` — detail of one review
- `/sat retry <review_id>` — retry a failed review, or re-run after the PR head changed
- `/sat refresh <review_id|all>` — archive merged/closed PRs, complete their task, sync reviewer comments to the task
- `/sat act <review_id> <instruction>` — resume the review session to act on the PR
- `/sat threads <review_id>` — list all chat threads the review was requested in
- `/sat describe review <id>` — full details including runs, reactions and threads
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
