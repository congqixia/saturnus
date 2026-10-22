# Tasks Module Plan

## Goal

Add a `Tasks` module to Saturnus that tracks a feature/effort end to end by
binding it to **one GitHub issue**, the **local design docs** that define it,
and the **PRs** that implement it. For every linked PR the module independently
tracks progress: PR state (open/closed/merged), review status, and the
corresponding **Feishu task** created by the PR review pipeline. The whole
surface is queryable from the Lark bot (`/sat task ...`) and the web UI.

This mirrors the review module's shape: a `tasks.Service` in a new
`internal/tasks` package, three new SQLite tables, `/sat` command arms, HTTP
API endpoints, and a web panel.

## Naming

"Task" here is a **feature tracking record** (ID prefix `tsk_`), distinct from
the Feishu tasks the review pipeline creates. To disambiguate, Feishu task
references in this module are named `fs_task_id` / `fs_task_url` (like
`review_requests.task_id` / `task_url`). Store types use `Task`, `TaskDoc` and
`TaskPR`; the package is `internal/tasks`.

## Capabilities

1. Create a task from a GitHub issue URL: `https://github.com/<owner>/<repo>/issues/<n>`.
2. Attach local design docs — repo-relative paths inside whitelisted checkouts.
3. Attach PRs — `https://github.com/<owner>/<repo>/pull/<n>`.
4. Per-PR progress tracking, **standalone**: each `task_prs` row keeps its own
   snapshot of PR state, review status, and Feishu task id/url, refreshed on
   demand. The snapshot is copied from the linked `review_requests` row (via
   `review_id`) when one exists; it never depends on live review lookups.
5. Refresh (`/sat task refresh <id|all>`) re-syncs issue state, PR states and
   per-PR review/Feishu-task snapshots from `gh` and the store.
6. Archive/close tasks; archived tasks are hidden from default listings.
7. Query via `/sat` commands and the web UI.

The module is track-only: attaching a PR does **not** start a review. If a PR
has no review yet its `review_status` is `none` and the bot hints at
`/sat review <pr-url>`. A later refresh picks up a review created through the
normal review pipeline.

## Runtime Components

### Server

Everything lives in `cmd/saturnus-server`. A new `internal/tasks` package is
wired into the `server` struct next to `reviews`. No new binary; no worker pool
is required (all work is synchronous `gh`/store calls, refresh can be batched
but stays request-scoped).

### GitHub resolution

Reuses the existing `gh` CLI conventions from `internal/review/github.go`:

- `gh issue view <n> --repo <owner/repo> --json number,title,state,url`
- `gh pr view <n> --repo <owner/repo> --json number,title,state,url,baseRefName,headRefOid`

The repo whitelist is the **shared** `SATURNUS_REVIEW_REPOS` map
(`owner/repo=/local/path`), so docs and PRs are only accepted for repos Saturnus
already knows a local checkout for.

### Lark bot

New `/sat task ...` command arms in `handleBotCommand`, plus entries in
`satCommandList` and the README. Replies use the existing `botReply`
(text / `TableCard` / `MarkdownCard`) helpers.

## Data Model

New tables in `internal/store` (added to `migrate()`; additive `ensureColumn`
calls for any columns added later).

### tasks

One row per tracked feature/effort.

| column | type | note |
|---|---|---|
| id | TEXT PK | `tsk_...` |
| title | TEXT | human-readable title (from issue or user) |
| status | TEXT | open / in_progress / done / archived |
| issue_repo | TEXT | `owner/repo` hosting the issue |
| issue_number | INTEGER | GitHub issue number |
| issue_title | TEXT | snapshot from `gh issue view` |
| issue_state | TEXT | OPEN / CLOSED snapshot |
| issue_url | TEXT | canonical issue URL |
| description | TEXT | free-form notes (optional) |
| archived_at | TEXT | set on close/archive; hidden from default listings |
| created_at / updated_at | TEXT | |

Indexes: `created_at DESC`, `status`.

### task_docs

Attached local design docs. A doc is a repo-relative path inside the mapped
checkout of a whitelisted repo.

| column | type | note |
|---|---|---|
| id | TEXT PK | `tdc_...` |
| task_id | TEXT FK | -> tasks, ON DELETE CASCADE |
| repo | TEXT | `owner/repo` hosting the doc |
| path | TEXT | repo-relative path, e.g. `docs/plan/foo.md` |
| title | TEXT | optional display title; defaults to file base name |
| created_at | TEXT | |

`UNIQUE(task_id, repo, path)`.

### task_prs

Per-PR progress, tracked standalone.

| column | type | note |
|---|---|---|
| id | TEXT PK | `tpr_...` |
| task_id | TEXT FK | -> tasks, ON DELETE CASCADE |
| repo | TEXT | `owner/repo` |
| pr_number | INTEGER | |
| pr_url | TEXT | canonical PR URL |
| pr_title | TEXT | snapshot from `gh pr view` |
| pr_state | TEXT | OPEN / CLOSED / MERGED snapshot |
| base_branch | TEXT | merge target |
| review_id | TEXT | link to `review_requests` when a review exists (traceability; empty otherwise) |
| review_status | TEXT | snapshot of the linked review's status, or `none` |
| fs_task_id | TEXT | Feishu task GUID snapshot (from the linked review) |
| fs_task_url | TEXT | clickable Feishu task applink snapshot |
| synced_at | TEXT | last snapshot refresh time |
| created_at / updated_at | TEXT | |

`UNIQUE(task_id, repo, pr_number)`.

The snapshot fields are written on attach and on refresh by copying from the
linked `review_requests` row (`GetReviewByPR(repo, pr_number)`). Listing and
detail rendering read only the snapshot columns, so a live review lookup is
never required to display a task — this is the "standalone per-PR tracking"
choice.

## Flow

### Create from issue

1. Parse the GitHub issue URL (`github.com/<owner>/<repo>/issues/<n>`).
2. Validate `<owner>/<repo>` is in the repo whitelist; reject otherwise.
3. `gh issue view` resolves title/state/url (graceful fallback to the URL when
   `gh` is missing or the issue is unreachable).
4. Insert the `tasks` row (`status=open`) and ack `Task created tsk_xxx`.

### Attach a design doc

1. `add-doc <task_id> <owner/repo>:<path>`.
2. Resolve the checkout dir from the whitelist and verify
   `<dir>/<path>` exists (defense in depth; a missing file is rejected).
3. Insert the `task_docs` row (deduped by `(task_id, repo, path)`).

### Attach a PR

1. Parse the PR URL; validate the repo whitelist.
2. `gh pr view` resolves title/state/base branch.
3. Look up `GetReviewByPR(repo, pr_number)`: if a review exists, snapshot
   `review_id`, `review_status` and the review's `task_id`/`task_url` into
   `fs_task_id`/`fs_task_url`; otherwise `review_status=none`.
4. Insert/update the `task_prs` row. No review is started.

### Refresh

`/sat task refresh <task_id>` (or `all` to sweep non-archived tasks):

1. Re-resolve the issue (`gh issue view`) and update `issue_title`/`issue_state`.
2. For each PR: re-resolve `gh pr view` state; re-lookup the linked review and
   refresh `review_status` + `fs_task_id`/`fs_task_url` snapshots; bump
   `synced_at`.
3. If the issue is `CLOSED`, or every PR is `MERGED` (and at least one exists),
   suggest closing the task; the bot reply surfaces this. (`/sat task close`
   does the actual archive.)

### Close / archive

`/sat task close <task_id>` sets `status=archived` and `archived_at`. Archived
tasks reject further doc/PR additions (`ErrArchived`, mirroring the review
module) and are hidden from default listings.

## Store API

Following the `CreateReview`/`UpdateReview` transaction + audit pattern in
`internal/store/store.go`:

- `CreateTask(Task)`, `GetTask(id)`, `ListTasks(status string, includeArchived bool)`, `UpdateTask(Task)`, `CloseTask(id)`
- `AddTaskDoc(TaskDoc)`, `ListTaskDocs(taskID)`, `RemoveTaskDoc(id)`
- `AddTaskPR(TaskPR)`, `GetTaskPR(id)`, `ListTaskPRs(taskID)`, `UpdateTaskPR(TaskPR)`, `RemoveTaskPR(id)`
- `scanTask`/`scanTaskDoc`/`scanTaskPR` helpers + scanner interfaces; ID prefixes `tsk_`/`tdc_`/`tpr_`; sentinel `ErrNotFound` reuse.

## Config

| env | default | note |
|---|---|---|
| SATURNUS_TASKS_ENABLED | 0 | master switch; `1` enables |
| SATURNUS_TASKS_GH_CLI | gh | GitHub CLI for issue/PR resolution |

Repo whitelist is shared via `SATURNUS_REVIEW_REPOS`. `/sat task ...` commands
are gated by the existing `SATURNUS_COMMAND_ALLOWED_USERS` access control.

## API Additions

- `GET /api/tasks?status=&archived=` — list tasks
- `GET /api/tasks/{id}` — task + `docs` + `prs` (per-PR snapshots)
- `POST /api/tasks` — create from `{"issue_url": ...}`
- `POST /api/tasks/{id}/docs` — attach `{"repo": ..., "path": ...}`
- `DELETE /api/tasks/{id}/docs/{doc_id}`
- `POST /api/tasks/{id}/prs` — attach `{"pr_url": ...}`
- `DELETE /api/tasks/{id}/prs/{pr_id}`
- `POST /api/tasks/{id}/refresh`
- `PATCH /api/tasks/{id}` — update status/title/description, or close

Handlers follow the `listReviews`/`getReview` pattern (`writeJSON` /
`writeStoreError`, `r.PathValue`).

## Bot Commands

- `/sat tasks` — list active tasks (status tag, issue, PR count)
- `/sat tasks all|archived` — include archived, or only archived
- `/sat task <task_id>` — detail: issue, docs, and each PR's state + review status + Feishu task link
- `/sat task create <issue-url>` — create a task from a GitHub issue
- `/sat task add-doc <task_id> <owner/repo>:<path>` — attach a design doc
- `/sat task rm-doc <task_id> <doc_id>` — detach a design doc
- `/sat task add-pr <task_id> <pr-url>` — attach a PR (snapshots review/Feishu-task state if a review exists)
- `/sat task rm-pr <task_id> <pr_id>` — detach a PR
- `/sat task refresh <task_id|all>` — re-sync issue/PR/review/Feishu-task snapshots
- `/sat task close <task_id>` — archive the task

Task detail renders as a `TableCard` (native Feishu table): one row per PR with
columns PR, state tag, review tag, Feishu task link.

## Web UI

A **Tasks** panel mirrors the existing PR Reviews panel (`web/src/App.vue`,
`controlPlane.ts`, `api.ts`):

- `api.ts`: `Task`, `TaskDoc`, `TaskPR` types mirroring the Go structs.
- `controlPlane.ts`: `tasks` state, `selectTask()` action, fetch `/api/tasks`
  in `refresh()`.
- `App.vue`: `NDataTable` listing tasks; clicking one loads
  `GET /api/tasks/{id}` and shows the issue summary, the docs list, and a PRs
  table with colored tags for PR state and review status plus a Feishu task link.

## Security Notes

- Only allowlisted users can run `/sat task ...` commands.
- Docs and PRs are restricted to whitelisted repos; doc paths must exist inside
  the mapped checkout (no arbitrary host paths, no checkout performed).
- GitHub issue/PR resolution uses host `gh` credentials; nothing is injected
  into subprocesses.
- Task creation from an issue URL never fetches code; only metadata via `gh`.

## Milestones

1. Store schema + `internal/tasks` service (create/list/get/attach/close) + API.
2. GitHub resolution (`gh issue view`, `gh pr view`) + refresh with per-PR
   review/Feishu-task snapshotting.
3. `/sat task ...` commands + README.
4. Web UI panel.
