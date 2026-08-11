import { defineStore } from 'pinia'
import { api, type Approval, type Event, type Session } from '../api'

type Health = {
  ok: boolean
  time?: string
  error?: string
}

export const useControlPlaneStore = defineStore('controlPlane', {
  state: () => ({
    health: { ok: false } as Health,
    sessions: [] as Session[],
    pending: [] as Approval[],
    selectedId: '',
    selected: null as Session | null,
    events: [] as Event[],
    loading: false,
    error: ''
  }),
  getters: {
    activeSessions: (state) => state.sessions.filter((session) => session.status === 'active').length,
    pendingCount: (state) => state.pending.length
  },
  actions: {
    async refresh() {
      this.loading = true
      this.error = ''
      try {
        this.health = await api<Health>('/api/health')
        const [sessions, approvals] = await Promise.all([
          api<{ sessions: Session[] }>('/api/sessions'),
          api<{ approvals: Approval[] }>('/api/approvals?status=pending')
        ])
        this.sessions = sessions.sessions ?? []
        this.pending = approvals.approvals ?? []
        if (!this.selectedId && this.sessions.length > 0) {
          this.selectedId = this.sessions[0].id
        }
        if (this.selectedId) {
          await this.selectSession(this.selectedId, false)
        }
      } catch (error) {
        this.health = { ok: false }
        this.error = error instanceof Error ? error.message : String(error)
      } finally {
        this.loading = false
      }
    },
    async selectSession(id: string, updateSelectedId = true) {
      if (updateSelectedId) this.selectedId = id
      const detail = await api<{ session: Session; events: Event[] }>(`/api/sessions/${encodeURIComponent(id)}`)
      this.selected = detail.session
      this.events = detail.events ?? []
    },
    async setAutoPass(enabled: boolean) {
      if (!this.selected) return
      await api<{ session: Session }>(`/api/sessions/${encodeURIComponent(this.selected.id)}/autopass`, {
        method: 'PATCH',
        body: JSON.stringify({ enabled, ttl: '30m', scope: 'web', actor: 'web' })
      })
      await this.refresh()
    },
    async decide(id: string, decision: 'approved' | 'denied') {
      await api(`/api/approvals/${encodeURIComponent(id)}/decision`, {
        method: 'POST',
        body: JSON.stringify({ decision, decided_by: 'web', source: 'web' })
      })
      await this.refresh()
    }
  }
})
