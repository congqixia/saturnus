# Task Background Refresh

Status: implemented.

## Existing data and behavior

A feature task (`tasks`, ID prefix `tsk_`) binds a GitHub issue to local design
references (`task_docs`) and implementing PRs (`task_prs`). Each PR association
stores independent snapshots of GitHub metadata, the latest local review, and
its Feishu task references. A PR can belong to multiple tasks; uniqueness is
only enforced within a task by `(task_id, repo, pr_number)`.

`review_requests` stores actual review executions. Its `task_id` is a Feishu
GUID, not a feature task ID. `head_commit` identifies the reviewed revision;
`pr_head_commit` identifies the most recently observed PR head.

Previously, task refresh fetched the issue, discovered same-repository PRs from
the issue timeline, fetched linked PR metadata, and copied local review
snapshots. It did not refresh the reviews themselves. Review refresh separately
updated the observed head, archived closed/merged PRs, restored reopened PRs,
and synchronized Feishu tasks. Web polling only reads SQLite.

The local default database had no task/review tables during design inspection;
production counts and request volume were not available. The first version
targets one server process with SQLite and serial refresh work.

## Scope

Automatic scans select `open` and `in_progress` tasks. `done` and `archived`
tasks are excluded. Manual refresh retains its non-archived scope. All PRs of
selected tasks are checked, including closed PRs, so reopening can be detected.
Archived feature tasks are not monitored.

Refresh updates issue metadata, discovers PR associations, fetches PR metadata,
synchronizes existing reviews when that module is enabled, and then snapshots
the resulting local review state. Missing reviews remain `none`. It never
starts AI reviews, publishes GitHub comments, or automatically changes a feature
task to done/archived. A changed head updates `pr_head_commit` without replacing
the reviewed baseline, allowing existing OUTDATED indicators to work.

Only reviews associated with selected tasks are included. Feishu synchronization
uses the existing review behavior when configured; disabled reviews do not
prevent GitHub metadata refresh. Reopening a PR retains existing review restore
semantics and does not automatically reopen a Feishu task.

## Scheduling and ordering

Run a background goroutine inside the existing server, separate from AI review
workers. No external scheduler, queue, or additional binary is needed.

| Configuration | Default | Meaning |
| --- | --- | --- |
| `SATURNUS_TASKS_ENABLED` | existing | Module switch |
| `SATURNUS_TASKS_REFRESH_INTERVAL` | `5m` | Delay after each scan; `0` disables scheduling |
| `SATURNUS_TASKS_REFRESH_TIMEOUT` | `60s` | Positive per-task refresh budget |

Reject malformed or negative intervals and nonpositive timeouts at startup.
An enabled scheduler performs an immediate asynchronous scan, waits for it to
finish, then waits the configured interval. Scans never overlap or accumulate
missed ticks. Process tasks and PRs serially until observed workload justifies
bounded concurrency.

```text
Read and validate task
  -> Fetch issue metadata and linked PR timeline
  -> Attach newly discovered PRs without redundant metadata requests
  -> For each PR:
       Fetch GitHub metadata, including headRefOid
       -> Synchronize an existing review when enabled
       -> Read the current local review and save the task PR snapshot
  -> Save issue snapshot and structured refresh result
```

Share PR fetch and review synchronization results by normalized `(repo, number)`
within each scan. Visit least recently attempted PRs first so a task that hits
its budget resumes with unfinished work on the next scan. Update every
association even when a PR is shared. Cache only
within the scan. Manual requests use the same core with their own cache scope.

Inject a review synchronization callback into the task service. Wire its adapter
in the server so neither service imports the other. Pass already fetched PR
metadata to the review core to avoid a second GitHub lookup. Keep sender checks
on manual review entry points; do not fabricate a sender for background work.

## Correctness requirements

### Failures and freshness

Strict GitHub lookups must return errors for command failures, timeouts, invalid
JSON, and invalid payloads. Empty timelines and failed timeline requests must be
distinguishable. Creation may retain its existing best-effort fallback.

Continue after an individual issue, timeline, or PR failure. Preserve failed
parts of the last snapshot while saving successful parts. Local review snapshots
may still update after a failed GitHub lookup, without advancing the PR's GitHub
success timestamp. Stop further work on cancellation.

Use structured results with `success`, `partial`, `failed`, or `skipped`, stage
errors, PR counts, and an archive suggestion. Suggestions are not errors.

### Concurrent updates

Refresh must update only its own snapshot/synchronization fields. Never write
back a stale full task/review row after waiting on the network. Preserve manual
titles, descriptions, statuses, requester identity, review output, verdicts, and
reviewed head commits. Guard association writes against parent task closure or
changes outside the automatic selection scope.

Serialize refreshes of the same task and review synchronization of the same PR
with cancellable locks. Coordinate review retry transitions with synchronization;
skip lifecycle and Feishu synchronization for pending/running reviews until a
later scan. Do not hold SQLite transactions during external calls.

### Retriable external effects

For closed/merged PRs, check the actual Feishu completion state rather than only
attempting completion on the initial local archive transition. Retry failures
on subsequent scans, including when the local review is already archived.

Fetch submitted GitHub reviews with pagination. Keep the existing submitted-time
watermark, and put a stable GitHub review ID marker in appended descriptions to
avoid duplication if the remote write succeeds but the local marker save fails.
Do not claim an exactly-once guarantee across SQLite and Feishu. Respect the
remote description size limit rather than silently losing unsynchronized items.

Propagate cancellation through Feishu reads, writes, and token acquisition.
Protect the shared token cache from concurrent bot, worker, and refresh requests.

### Shutdown and isolation

Propagate the caller's context through issue/timeline/PR/review/Feishu work and
automatic associations. Bound individual GitHub subprocesses to 15 seconds
within the remaining task budget. A failing task must not abort the sweep.
Retry external failures on a later scan, without tight retry loops.

On shutdown, cancel and join the scheduler before stopping review services and
closing SQLite. HTTP refresh work must also drain or cancel safely.

## Persistence and visibility

Use additive `ensureColumn` migrations and retain existing association tables.

| Table | Field | Meaning |
| --- | --- | --- |
| `tasks` | `refresh_attempted_at` | Latest task refresh start |
| `tasks` | `refreshed_at` | Latest fully successful task refresh completion |
| `tasks` | `refresh_error` | Latest stage errors, cleared on full success |
| `task_prs` | `sync_attempted_at` | Latest GitHub lookup attempt |
| `task_prs` | `sync_error` | Latest metadata or review synchronization error |

`task_prs.synced_at` means the GitHub metadata was fetched and saved successfully.
A partial refresh does not advance `tasks.refreshed_at`. Disabled/missing reviews
are not failures; running reviews deferred to another scan make the task partial.
Old rows begin without a success timestamp. Do not interpret `updated_at` as a
successful remote refresh.

Expose freshness and errors in GET API, Web, and bot task details. Keep automatic
scans quiet in chat. Show archive suggestions in task details. Log sweep duration,
task and unique PR counts, outcomes, and stage errors. Avoid full-row audit events
for unchanged polling snapshots.

## Implementation and validation

1. Introduce strict/context-aware lookups, structured results, narrow writes,
   and concurrency protection; route manual refresh through the same core.
2. Wire review synchronization before snapshotting; add retryable completion,
   stable review markers, and per-scan shared PR results.
3. Add scheduling, configuration validation, shutdown joining, additive schema,
   and API/Web/bot freshness display.

Use fake GitHub/Lark clients and controlled scheduler waits to cover active task
selection, discovery, shared PR deduplication, head changes, closed/reopened PRs,
disabled reviews, partial errors, pagination, cancellation, concurrent edits and
retries, failed remote completion, and remote-success/local-failure retries.
Run the relevant Go suites with the race detector and build the frontend.

If scanning approaches the interval, use measured logs to justify bounded
concurrency, backoff, or slower terminal PR checks. Multiple server processes
would require a database lease; in-process locks alone do not support that mode.

## Validation notes

The implementation includes fake-process/service coverage for active and manual
scopes, shared PRs, freshness preservation, concurrent edits and closure, retry
serialization, paginated review reads, remote-success/local-conflict deduplication,
cancellation and joining, API errors, token cache races, and legacy migration.
Validation uses local fakes; it does not mutate real GitHub or Feishu resources.
