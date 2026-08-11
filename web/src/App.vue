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
import { formatTime, pretty, type Approval, type Event, type Session } from './api'
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
