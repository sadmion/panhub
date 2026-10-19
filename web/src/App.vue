<script setup lang="ts">
import { onMounted, ref } from 'vue'
import Login from './views/Login.vue'
import Browser from './views/Browser.vue'
import Mounts from './views/Mounts.vue'
import Tasks from './views/Tasks.vue'
import { api, clearToken, getToken } from './api'

const authed = ref(!!getToken())
const tab = ref<'browser' | 'mounts' | 'tasks'>('browser')
const status = ref<any>({})

const tabs = [
  { key: 'browser', label: '文件浏览', icon: '📁' },
  { key: 'mounts', label: '挂载管理', icon: '🔌' },
  { key: 'tasks', label: 'STRM 任务', icon: '⏱️' },
] as const

async function loadStatus() {
  try {
    status.value = await api.get('/status')
  } catch {
    clearToken()
    authed.value = false
  }
}

function onLogin() {
  authed.value = true
  loadStatus()
}

function logout() {
  clearToken()
  authed.value = false
}

onMounted(() => {
  if (authed.value) loadStatus()
})
</script>

<template>
  <Login v-if="!authed" @ok="onLogin" />

  <div v-else class="shell">
    <aside class="sidebar">
      <div class="brand">panhub<small>网盘聚合挂载</small></div>
      <button
        v-for="t in tabs"
        :key="t.key"
        class="nav-item"
        :class="{ active: tab === t.key }"
        @click="tab = t.key"
      >
        <span>{{ t.icon }}</span><span>{{ t.label }}</span>
      </button>
      <button class="nav-item" @click="logout"><span>↩</span><span>退出登录</span></button>
      <div class="sidebar-foot">
        v{{ status.version }}<br />
        {{ status.mounts ?? 0 }} 个挂载 · {{ status.tasks ?? 0 }} 个任务<br />
        <span class="mono">{{ status.base_url }}</span>
      </div>
    </aside>

    <main class="main">
      <Browser v-if="tab === 'browser'" />
      <Mounts v-else-if="tab === 'mounts'" @changed="loadStatus" />
      <Tasks v-else />
    </main>
  </div>
</template>
