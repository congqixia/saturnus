<script setup lang="ts">
import { computed, onMounted, onUnmounted, type Component } from 'vue'
import {
  NButton,
  NConfigProvider,
  NDataTable,
  NDescriptions,
  NDescriptionsItem,
  NEmpty,
  NIcon,
  NLayout,
  NLayoutContent,
  NLayoutHeader,
  NLayoutSider,
  NScrollbar,
  NSpace,
  NTag,
  darkTheme,
  type DataTableColumns
} from 'naive-ui'
import { Check, RefreshCw, X, Zap, ZapOff } from 'lucide-vue-next'
import { formatTime, pretty, type Approval, type Event, type Review, type Session, type Task, type TaskPR } from './api'
import { useControlPlaneStore } from './stores/controlPlane'

const store = useControlPlaneStore()
let timer: number | undefined

onMounted(() => {
  void store.refresh()
  timer = window.setInterval(() => void store.refresh(), 5000)
})

onUnmounted(() => {
  if (timer) window.clearInterval(timer)
})

const sessionColumns = computed<DataTableColumns<Session>>(() => [
  {
    title: 'Session',
    key: 'id',
    render(row) {
      return row.id
    }
  },
  {
    title: 'Status',
    key: 'status',
    width: 96,
    render(row) {
      return row.status
    }
  },
  {
    title: 'Auto',
    key: 'auto_pass',
    width: 72,
    render(row) {
      return row.auto_pass ? 'On' : 'Off'
    }
  },
  {
    title: 'Updated',
    key: 'updated_at',
    width: 168,
    render(row) {
      return formatTime(row.updated_at)
    }
  }
])

const approvalColumns = computed<DataTableColumns<Approval>>(() => [
  {
    title: 'Request',
    key: 'id',
    width: 180
  },
  {
    title: 'Session',
    key: 'session_id',
    width: 180
  },
  {
    title: 'Tool',
    key: 'tool_name',
    width: 160
  },
  {
    title: 'Risk',
    key: 'risk_level',
    width: 100,
    render(row) {
      return row.risk_level
    }
  },
  {
    title: 'Actions',
    key: 'actions',
    width: 180,
    render(row) {
      return [
        hButton('Approve', Check, 'primary', () => void store.decide(row.id, 'approved')),
        hButton('Deny', X, 'error', () => void store.decide(row.id, 'denied'))
      ]
    }
  }
])

const eventColumns = computed<DataTableColumns<Event>>(() => [
  {
    title: 'Time',
    key: 'created_at',
    width: 180,
    render(row) {
      return formatTime(row.created_at)
    }
  },
  {
    title: 'Type',
    key: 'type',
    width: 180
  },
  {
    title: 'Payload',
    key: 'payload',
    render(row) {
      return pretty(row.payload)
    }
  }
])

const reviewColumns = computed<DataTableColumns<Review>>(() => [
  {
    title: 'Review',
    key: 'id',
    width: 180
  },
  {
    title: 'PR',
    key: 'repo',
    render(row) {
      return `${row.repo}#${row.pr_number}`
    }
  },
  {
    title: 'Status',
    key: 'status',
    width: 110,
    render(row) {
      return h(
        NTag,
        { size: 'small', round: true, type: reviewTagType(row.status) },
        { default: () => row.status }
      )
    }
  },
  {
    title: 'Updated',
    key: 'updated_at',
    width: 168,
    render(row) {
      return formatTime(row.updated_at)
    }
  }
])

function reviewTagType(status: string): 'success' | 'error' | 'warning' | 'default' {
  switch (status) {
    case 'succeeded':
      return 'success'
    case 'failed':
    case 'cancelled':
      return 'error'
    case 'reviewing':
    case 'pending':
      return 'warning'
    default:
      return 'default'
  }
}

function taskTagType(status: string): 'success' | 'error' | 'warning' | 'info' | 'default' {
  switch (status) {
    case 'done':
      return 'success'
    case 'in_progress':
      return 'warning'
    case 'open':
      return 'info'
    case 'archived':
      return 'default'
    default:
      return 'default'
  }
}

const taskColumns = computed<DataTableColumns<Task>>(() => [
  {
    title: 'Task',
    key: 'id',
    width: 180
  },
  {
    title: 'Title',
    key: 'title',
    render(row) {
      return row.title
    }
  },
  {
    title: 'Status',
    key: 'status',
    width: 110,
    render(row) {
      return h(
        NTag,
        { size: 'small', round: true, type: taskTagType(row.status) },
        { default: () => row.status }
      )
    }
  },
  {
    title: 'Issue',
    key: 'issue',
    render(row) {
      return row.issue_repo ? `${row.issue_repo}#${row.issue_number}` : '-'
    }
  },
  {
    title: 'Updated',
    key: 'updated_at',
    width: 168,
    render(row) {
      return formatTime(row.updated_at)
    }
  }
])

const taskPrColumns = computed<DataTableColumns<TaskPR>>(() => [
  {
    title: 'PR',
    key: 'pr',
    render(row) {
      return `${row.repo}#${row.pr_number}`
    }
  },
  {
    title: 'State',
    key: 'pr_state',
    width: 110,
    render(row) {
      return h(
        NTag,
        { size: 'small', round: true, type: prStateTagType(row.pr_state) },
        { default: () => row.pr_state || 'unknown' }
      )
    }
  },
  {
    title: 'Review',
    key: 'review_status',
    width: 110,
    render(row) {
      return h(
        NTag,
        { size: 'small', round: true, type: reviewTagType(row.review_status) },
        { default: () => row.review_status }
      )
    }
  },
  {
    title: 'Feishu Task',
    key: 'fs_task',
    render(row) {
      if (!row.fs_task_id) return '-'
      return row.fs_task_url
        ? h('a', { href: row.fs_task_url, target: '_blank' }, row.fs_task_id)
        : row.fs_task_id
    }
  }
])

function prStateTagType(state: string): 'success' | 'error' | 'warning' | 'default' {
  switch (state) {
    case 'MERGED':
      return 'success'
    case 'CLOSED':
      return 'error'
    case 'OPEN':
      return 'warning'
    default:
      return 'default'
  }
}

function hButton(
  label: string,
  icon: Component,
  type: 'primary' | 'error',
  onClick: () => void
) {
  return h(
    NButton,
    { size: 'small', type, secondary: true, onClick, style: 'margin-right: 8px' },
    {
      icon: () => h(NIcon, null, { default: () => h(icon) }),
      default: () => label
    }
  )
}
</script>

<script lang="ts">
import { h } from 'vue'
</script>

<template>
  <NConfigProvider :theme="null">
    <NLayout class="shell">
      <NLayoutHeader class="topbar" bordered>
        <div class="brand">
          <div class="brand-mark">S</div>
          <div>
            <h1>Saturnus</h1>
            <p>Agent sessions and approvals</p>
          </div>
        </div>
        <NSpace align="center" :size="10">
          <NTag :type="store.health.ok ? 'success' : 'error'" round>
            {{ store.health.ok ? 'Online' : 'Offline' }}
          </NTag>
          <NButton size="small" :loading="store.loading" @click="store.refresh">
            <template #icon>
              <NIcon><RefreshCw /></NIcon>
            </template>
            Refresh
          </NButton>
        </NSpace>
      </NLayoutHeader>

      <NLayout has-sider class="main">
        <NLayoutSider class="sidebar" bordered :width="360" collapse-mode="width">
          <div class="sidebar-head">
            <div>
              <strong>Sessions</strong>
              <span>{{ store.sessions.length }} total</span>
            </div>
            <NTag size="small" type="success" round>{{ store.activeSessions }} active</NTag>
          </div>
          <NScrollbar class="session-scroll">
            <button
              v-for="session in store.sessions"
              :key="session.id"
              class="session-row"
              :class="{ selected: store.selectedId === session.id }"
              @click="store.selectSession(session.id)"
            >
              <span class="row-main">
                <code>{{ session.id }}</code>
                <NTag :type="session.status === 'active' ? 'success' : 'default'" size="small" round>
                  {{ session.status }}
                </NTag>
              </span>
              <span class="muted">{{ session.cwd || 'No working directory' }}</span>
              <span class="muted">
                auto-pass {{ session.auto_pass ? 'on' : 'off' }} · {{ formatTime(session.updated_at) }}
              </span>
            </button>
            <NEmpty v-if="store.sessions.length === 0" class="empty-state" description="No sessions" />
          </NScrollbar>
        </NLayoutSider>

        <NLayoutContent>
          <main class="content">
            <section class="metrics">
              <div class="metric">
                <span>Sessions</span>
                <strong>{{ store.sessions.length }}</strong>
              </div>
              <div class="metric">
                <span>Active</span>
                <strong>{{ store.activeSessions }}</strong>
              </div>
              <div class="metric">
                <span>Pending</span>
                <strong>{{ store.pendingCount }}</strong>
              </div>
              <div class="metric">
                <span>Reviews</span>
                <strong>{{ store.reviewCount }}</strong>
              </div>
              <div class="metric">
                <span>Tasks</span>
                <strong>{{ store.taskCount }}</strong>
              </div>
              <div class="metric">
                <span>Events</span>
                <strong>{{ store.events.length }}</strong>
              </div>
            </section>

            <section class="panel">
              <div class="panel-head">
                <h2>Pending Approvals</h2>
                <NTag :type="store.pendingCount > 0 ? 'warning' : 'default'" round>
                  {{ store.pendingCount }}
                </NTag>
              </div>
              <NDataTable
                v-if="store.pending.length"
                :columns="approvalColumns"
                :data="store.pending"
                :bordered="false"
                :single-line="false"
              />
              <NEmpty v-else class="empty-state" description="No pending approvals" />
            </section>

            <section class="panel">
              <div class="panel-head">
                <h2>PR Reviews</h2>
                <NTag :type="store.reviewCount > 0 ? 'info' : 'default'" round>
                  {{ store.reviewCount }}
                </NTag>
              </div>
              <NDataTable
                v-if="store.reviews.length"
                :columns="reviewColumns"
                :data="store.reviews"
                :bordered="false"
                :single-line="false"
              />
              <NEmpty v-else class="empty-state" description="No reviews" />
              <template v-if="store.selectedReview">
                <NDescriptions bordered :column="2" label-placement="left" size="small" class="review-detail">
                  <NDescriptionsItem label="Repo">
                    <code>{{ store.selectedReview.repo }}#{{ store.selectedReview.pr_number }}</code>
                  </NDescriptionsItem>
                  <NDescriptionsItem label="Status">
                    {{ store.selectedReview.status }}
                  </NDescriptionsItem>
                  <NDescriptionsItem label="Requester">
                    {{ store.selectedReview.requester_name || store.selectedReview.requester_open_id || '-' }}
                  </NDescriptionsItem>
                  <NDescriptionsItem label="Tool">
                    {{ store.selectedReview.tool || '-' }}
                  </NDescriptionsItem>
                  <NDescriptionsItem label="Task">
                    {{ store.selectedReview.task_id || '-' }}
                  </NDescriptionsItem>
                  <NDescriptionsItem label="Updated">
                    {{ formatTime(store.selectedReview.updated_at) }}
                  </NDescriptionsItem>
                </NDescriptions>
                <div v-if="store.selectedReview.error" class="review-error">
                  {{ store.selectedReview.error }}
                </div>
                <pre v-if="store.selectedReview.result_text" class="review-result">{{ store.selectedReview.result_text }}</pre>
              </template>
            </section>

            <section class="panel">
              <div class="panel-head">
                <h2>Tasks</h2>
                <NTag :type="store.taskCount > 0 ? 'info' : 'default'" round>
                  {{ store.taskCount }}
                </NTag>
              </div>
              <NDataTable
                v-if="store.tasks.length"
                :columns="taskColumns"
                :data="store.tasks"
                :bordered="false"
                :single-line="false"
                :row-key="(row: Task) => row.id"
                :row-props="(row: Task) => ({ onClick: () => store.selectTask(row.id), style: 'cursor: pointer' })"
              />
              <NEmpty v-else class="empty-state" description="No tasks" />
              <template v-if="store.selectedTask">
                <NDescriptions bordered :column="2" label-placement="left" size="small" class="review-detail">
                  <NDescriptionsItem label="Task">
                    <code>{{ store.selectedTask.id }}</code>
                  </NDescriptionsItem>
                  <NDescriptionsItem label="Status">
                    {{ store.selectedTask.status }}
                  </NDescriptionsItem>
                  <NDescriptionsItem label="Issue">
                    <template v-if="store.selectedTask.issue_url">
                      <a :href="store.selectedTask.issue_url" target="_blank">
                        {{ store.selectedTask.issue_repo }}#{{ store.selectedTask.issue_number }}
                      </a>
                    </template>
                    <span v-else>-</span>
                  </NDescriptionsItem>
                  <NDescriptionsItem label="Updated">
                    {{ formatTime(store.selectedTask.updated_at) }}
                  </NDescriptionsItem>
                </NDescriptions>
                <div v-if="store.selectedTaskDocs.length" class="task-docs">
                  <strong>Design docs</strong>
                  <ul>
                    <li v-for="doc in store.selectedTaskDocs" :key="doc.id">
                      <code>{{ doc.repo }}:{{ doc.path }}</code>
                    </li>
                  </ul>
                </div>
                <div v-if="store.selectedTaskPrs.length" class="task-prs">
                  <strong>Linked PRs</strong>
                  <NDataTable
                    :columns="taskPrColumns"
                    :data="store.selectedTaskPrs"
                    :bordered="false"
                    :single-line="false"
                    size="small"
                  />
                </div>
              </template>
            </section>

            <section class="panel">
              <div class="panel-head">
                <h2>Session Detail</h2>
                <NSpace v-if="store.selected" :size="8">
                  <NButton size="small" type="primary" secondary :disabled="store.selected.auto_pass" @click="store.setAutoPass(true)">
                    <template #icon>
                      <NIcon><Zap /></NIcon>
                    </template>
                    Auto-pass On
                  </NButton>
                  <NButton size="small" secondary :disabled="!store.selected.auto_pass" @click="store.setAutoPass(false)">
                    <template #icon>
                      <NIcon><ZapOff /></NIcon>
                    </template>
                    Auto-pass Off
                  </NButton>
                </NSpace>
              </div>
              <template v-if="store.selected">
                <NDescriptions bordered :column="2" label-placement="left" size="small">
                  <NDescriptionsItem label="Session">
                    <code>{{ store.selected.id }}</code>
                  </NDescriptionsItem>
                  <NDescriptionsItem label="Agent">
                    <code>{{ store.selected.agent_id }}</code>
                  </NDescriptionsItem>
                  <NDescriptionsItem label="Status">
                    {{ store.selected.status }}
                  </NDescriptionsItem>
                  <NDescriptionsItem label="Auto-pass">
                    {{ store.selected.auto_pass ? 'On' : 'Off' }}
                  </NDescriptionsItem>
                  <NDescriptionsItem label="CWD">
                    <code>{{ store.selected.cwd || '-' }}</code>
                  </NDescriptionsItem>
                  <NDescriptionsItem label="Model">
                    <code>{{ store.selected.model || '-' }}</code>
                  </NDescriptionsItem>
                  <NDescriptionsItem label="Auto-pass Until">
                    {{ formatTime(store.selected.auto_pass_until) }}
                  </NDescriptionsItem>
                  <NDescriptionsItem label="Heartbeat">
                    {{ formatTime(store.selected.last_heartbeat_at) }}
                  </NDescriptionsItem>
                </NDescriptions>
              </template>
              <NEmpty v-else class="empty-state" description="Select a session" />
            </section>

            <section class="panel">
              <div class="panel-head">
                <h2>Timeline</h2>
                <NTag round>{{ store.events.length }}</NTag>
              </div>
              <NDataTable
                v-if="store.events.length"
                :columns="eventColumns"
                :data="store.events"
                :bordered="false"
                :single-line="false"
              />
              <NEmpty v-else class="empty-state" description="No events" />
            </section>

            <section class="panel compact">
              <div class="panel-head">
                <h2>All Sessions</h2>
              </div>
              <NDataTable
                :columns="sessionColumns"
                :data="store.sessions"
                :bordered="false"
                :single-line="false"
              />
            </section>
          </main>
        </NLayoutContent>
      </NLayout>
    </NLayout>
  </NConfigProvider>
</template>
