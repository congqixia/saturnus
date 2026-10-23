export type Session = {
  id: string
  agent_id: string
  cwd: string
  repo: string
  branch: string
  model: string
  status: string
  auto_pass: boolean
  auto_pass_until?: string
  auto_pass_scope?: string
  created_at: string
  updated_at: string
  last_heartbeat_at?: string
}

export type Event = {
  id: string
  session_id: string
  turn_id?: string
  type: string
  payload: unknown
  created_at: string
}

export type Approval = {
  id: string
  session_id: string
  tool_name: string
  tool_input: unknown
  risk_level: string
  status: string
  reason?: string
  created_at: string
  expires_at: string
}

export type Review = {
  id: string
  thread_id: string
  status: string
  pr_url: string
  repo: string
  pr_number: number
  title: string
  base_branch: string
  requester_open_id: string
  requester_name: string
  chat_id: string
  message_id: string
  tool: string
  task_id: string
  task_url: string
  session_id: string
  head_commit: string
  retry_reason: string
  result_text: string
  error: string
  verdict?: string
  summary?: string
  comments_posted_at?: string
  created_at: string
  updated_at: string
  completed_at?: string
}

export type ReviewRun = {
  id: string
  review_id: string
  seq: number
  status: string
  head_commit: string
  retry_reason: string
  session_id: string
  result_text: string
  error: string
  tool: string
  verdict?: string
  summary?: string
  started_at: string
  completed_at?: string
  updated_at: string
}

// ReviewFinding is one concrete issue the review tool flagged, graded P0-P2.
// Status tracks it across review rounds (open/addressed/commented/resolved);
// github_comment_id links it to the inline comment posted on the PR.
export type ReviewFinding = {
  id: string
  review_id: string
  run_id?: string
  severity: string
  path: string
  line: number
  title: string
  message: string
  suggestion: string
  status: string
  github_comment_id?: string
  created_at: string
}

export type Task = {
  id: string
  title: string
  status: string
  issue_repo: string
  issue_number: number
  issue_title: string
  issue_state: string
  issue_url: string
  description: string
  archived_at?: string
  created_at: string
  updated_at: string
}

export type TaskDoc = {
  id: string
  task_id: string
  repo: string
  path: string
  title: string
  created_at: string
}

export type TaskPR = {
  id: string
  task_id: string
  repo: string
  pr_number: number
  pr_url: string
  pr_title: string
  pr_state: string
  base_branch: string
  review_id: string
  review_status: string
  fs_task_id: string
  fs_task_url: string
  synced_at?: string
  created_at: string
  updated_at: string
}

export async function api<T>(path: string, options: RequestInit = {}): Promise<T> {
  const response = await fetch(path, {
    headers: { 'Content-Type': 'application/json', ...(options.headers ?? {}) },
    ...options
  })
  if (!response.ok) {
    throw new Error(await response.text())
  }
  return response.json() as Promise<T>
}

export function formatTime(value?: string): string {
  if (!value || value.startsWith('0001-01-01')) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString()
}

export function pretty(value: unknown): string {
  if (!value) return '{}'
  if (typeof value === 'string') {
    try {
      return JSON.stringify(JSON.parse(value), null, 2)
    } catch {
      return value
    }
  }
  return JSON.stringify(value, null, 2)
}
