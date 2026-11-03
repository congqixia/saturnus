<script setup lang="ts">
import { ref, watch } from 'vue'
import { NAlert, NButton, NInput, NSelect, NSpace } from 'naive-ui'
import { api, type Task, type TaskSession } from './api'

const props = defineProps<{ task: Task | null; sessions: TaskSession[] }>()
const emit = defineEmits<{ created: [id: string, warning: string]; changed: [id: string] }>()

const showCreate = ref(false)
const createMode = ref('title')
const title = ref('')
const description = ref('')
const issueURL = ref('')
const bindingURL = ref('')
const tool = ref('codex')
const sessionID = ref('')
const purpose = ref('')
const note = ref('')
const prURL = ref('')
const busy = ref(false)
const error = ref('')

watch(() => props.task?.id, () => {
  bindingURL.value = ''
  sessionID.value = ''
  purpose.value = ''
  note.value = ''
  prURL.value = ''
  error.value = ''
})

async function perform(action: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try { await action() } catch (e) { error.value = e instanceof Error ? e.message : String(e) } finally { busy.value = false }
}

async function create() {
  await perform(async () => {
    const body = createMode.value === 'issue'
      ? { issue_url: issueURL.value.trim() }
      : { title: title.value.trim(), description: description.value }
    const result = await api<{ task: Task; lark_sync_error?: string }>('/api/tasks', { method: 'POST', body: JSON.stringify(body) })
    title.value = ''; description.value = ''; issueURL.value = ''; showCreate.value = false
    emit('created', result.task.id, result.lark_sync_error ?? '')
  })
}

async function bindIssue() {
  const id = props.task?.id
  if (!id) return
  await perform(async () => {
    await api(`/api/tasks/${encodeURIComponent(id)}/issue`, { method: 'PUT', body: JSON.stringify({ issue_url: bindingURL.value.trim() }) })
    bindingURL.value = ''
    emit('changed', id)
  })
}

async function addSession() {
  const id = props.task?.id
  if (!id) return
  await perform(async () => {
    await api(`/api/tasks/${encodeURIComponent(id)}/sessions`, { method: 'POST', body: JSON.stringify({ tool: tool.value, session_id: sessionID.value.trim(), purpose: purpose.value.trim(), note: note.value.trim() }) })
    sessionID.value = ''; purpose.value = ''; note.value = ''
    emit('changed', id)
  })
}

async function removeSession(linkID: string) {
  const id = props.task?.id
  if (!id) return
  await perform(async () => {
    await api(`/api/tasks/${encodeURIComponent(id)}/sessions/${encodeURIComponent(linkID)}`, { method: 'DELETE' })
    emit('changed', id)
  })
}

async function addPR() {
  const id = props.task?.id
  if (!id) return
  await perform(async () => {
    await api(`/api/tasks/${encodeURIComponent(id)}/prs`, { method: 'POST', body: JSON.stringify({ pr_url: prURL.value.trim() }) })
    prURL.value = ''
    emit('changed', id)
  })
}
</script>

<template>
  <div class="task-actions">
    <NAlert v-if="error" type="error" title="Task action failed" closable @close="error = ''">{{ error }}</NAlert>
    <NButton size="small" @click="showCreate = !showCreate">{{ showCreate ? 'Cancel creation' : 'New task' }}</NButton>
    <form v-if="showCreate" class="task-form" @submit.prevent="create">
      <NSelect v-model:value="createMode" aria-label="Task creation method" :options="[{ label: 'From title', value: 'title' }, { label: 'From GitHub issue', value: 'issue' }]" />
      <template v-if="createMode === 'title'">
        <NInput v-model:value="title" aria-label="Task title" placeholder="Task title" />
        <NInput v-model:value="description" aria-label="Task description" placeholder="Description (optional)" type="textarea" />
      </template>
      <NInput v-else v-model:value="issueURL" aria-label="Issue URL" placeholder="GitHub issue URL" />
      <NButton attr-type="submit" type="primary" size="small" :loading="busy" :disabled="createMode === 'title' ? !title.trim() : !issueURL.trim()">Create task</NButton>
    </form>

    <template v-if="task && task.status !== 'archived'">
      <form class="task-form" @submit.prevent="bindIssue">
        <strong>{{ task.issue_url ? 'Replace issue' : 'Bind issue' }}</strong>
        <NSpace :wrap="false">
          <NInput v-model:value="bindingURL" aria-label="Issue URL to bind" placeholder="GitHub issue URL" />
          <NButton attr-type="submit" size="small" :loading="busy" :disabled="!bindingURL.trim()">Bind</NButton>
        </NSpace>
      </form>
      <form class="task-form" @submit.prevent="addPR">
        <strong>Link PR</strong>
        <NSpace :wrap="false">
          <NInput v-model:value="prURL" aria-label="PR URL" placeholder="GitHub PR URL" />
          <NButton attr-type="submit" size="small" :loading="busy" :disabled="!prURL.trim()">Link</NButton>
        </NSpace>
      </form>
      <form class="task-form" @submit.prevent="addSession">
        <strong>Link session</strong>
        <NSpace :wrap="true">
          <NSelect v-model:value="tool" aria-label="Session tool" class="task-tool" :options="[{ label: 'Codex', value: 'codex' }, { label: 'OpenCode', value: 'opencode' }]" />
          <NInput v-model:value="sessionID" aria-label="Session ID" placeholder="Session ID" />
          <NInput v-model:value="purpose" aria-label="Session purpose" placeholder="Purpose (optional)" />
          <NInput v-model:value="note" aria-label="Session note" placeholder="Note (optional)" />
          <NButton attr-type="submit" size="small" :loading="busy" :disabled="!sessionID.trim()">Link</NButton>
        </NSpace>
      </form>
    </template>
    <div v-if="task && sessions.length" class="task-form">
      <strong>Sessions</strong>
      <div v-for="session in sessions" :key="session.id" class="task-session-row">
        <span><code>{{ session.tool }}:{{ session.session_id }}</code><span v-if="session.purpose"> — {{ session.purpose }}</span><span v-if="session.note"> — {{ session.note }}</span></span>
        <NButton v-if="task.status !== 'archived'" size="tiny" :disabled="busy" @click="removeSession(session.id)">Unlink</NButton>
      </div>
    </div>
  </div>
</template>
