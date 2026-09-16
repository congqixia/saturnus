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
  session_id: string
  result_text: string
  error: string
  created_at: string
  updated_at: string
  completed_at?: string
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
