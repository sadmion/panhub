<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { api, fmtTime } from '../api'

const tasks = ref<any[]>([])
const mounts = ref<any[]>([])
const streamKey = ref('')
const error = ref('')
const notice = ref('')
const busy = ref(false)
const showForm = ref(false)
const testing = ref(false)

const form = reactive<any>({
  name: '',
  mount: '',
  dir: '/',
  target: '',
  mode: 'proxy',
  interval: '6h',
  enabled: true,
})

async function load() {
  error.value = ''
  try {
    const [t, m] = await Promise.all([api.get('/tasks'), api.get('/mounts')])
    tasks.value = t.tasks || []
    streamKey.value = t.stream_key || ''
    mounts.value = m.mounts || []
    if (!form.mount && mounts.value.length) form.mount = mounts.value[0].name
  } catch (e: any) {
    error.value = e.message
  }
}

function openForm() {
  form.name = ''
  form.dir = '/'
  form.target = ''
  form.mode = 'proxy'
  form.interval = '6h'
  form.enabled = true
  if (mounts.value.length) form.mount = mounts.value[0].name
  showForm.value = true
  notice.value = ''
}

// 测试输出目录：直接让后端试着建目录+写文件，避免任务跑完才发现写不进去
async function testTarget() {
  if (!form.target.trim()) {
    error.value = '先填输出目录'
    return
  }
  testing.value = true
  error.value = ''
  notice.value = ''
  try {
    const r = await api.post('/fs/check-target', { path: form.target })
    notice.value = `目录可写：${r.path}`
  } catch (e: any) {
    error.value = e.message
  } finally {
    testing.value = false
  }
}

async function submit() {
  busy.value = true
  error.value = ''
  try {
    await api.post('/tasks', {
      name: form.name,
      type: 'strm',
      mount: form.mount,
      dir: form.dir,
      target: form.target,
      mode: form.mode,
      interval: form.interval,
      enabled: form.enabled,
    })
    showForm.value = false
    notice.value = '任务已创建'
    await load()
  } catch (e: any) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}

async function runNow(t: any) {
  error.value = ''
  notice.value = `正在执行「${t.name}」…`
  try {
    const r = await api.post(`/tasks/${t.id}/run`)
    notice.value = r.note || `「${t.name}」执行完成`
    await load()
  } catch (e: any) {
    error.value = e.message
    notice.value = ''
  }
}

async function remove(t: any) {
  if (!confirm(`删除任务「${t.name}」？`)) return
  try {
    await api.del(`/tasks/${t.id}`)
    await load()
  } catch (e: any) {
    error.value = e.message
  }
}

onMounted(load)
</script>

<template>
  <div class="page-head">
    <h2>STRM 任务</h2>
    <div class="spacer" />
    <button class="primary" @click="openForm">＋ 新建任务</button>
  </div>

  <div v-if="error" class="alert error">{{ error }}</div>
  <div v-if="notice" class="alert ok">{{ notice }}</div>

  <div class="card" style="padding: 0">
    <table v-if="tasks.length">
      <thead>
        <tr>
          <th>任务</th>
          <th>挂载 / 目录</th>
          <th>输出</th>
          <th>间隔</th>
          <th>上次执行</th>
          <th style="width: 190px"></th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="t in tasks" :key="t.id">
          <td>
            <strong>{{ t.name }}</strong>
            <span class="tag" :class="{ on: t.enabled }">{{ t.enabled ? '启用' : '停用' }}</span>
          </td>
          <td class="muted">{{ t.mount }} : {{ t.dir }}</td>
          <td class="muted" style="max-width: 220px; word-break: break-all">{{ t.mode }} → {{ t.target }}</td>
          <td class="muted">{{ t.interval || '手动' }}</td>
          <td class="muted">
            <template v-if="t.last_run">
              {{ fmtTime(t.last_run) }}<br />
              <span :style="{ color: t.last_result === 'success' ? 'var(--ok)' : 'var(--danger)' }">
                {{ t.last_result }} · {{ t.last_count }} 个文件
              </span>
              <div v-if="t.last_target" class="mono" style="color: var(--muted); word-break: break-all">
                写到：{{ t.last_target }}
              </div>
              <div v-if="t.last_sample" class="mono" style="color: var(--ok); word-break: break-all">
                示例：{{ t.last_sample }}
              </div>
              <div v-if="t.last_error" class="mono" style="color: var(--danger); word-break: break-all">{{ t.last_error }}</div>
            </template>
            <template v-else>从未执行</template>
          </td>
          <td>
            <div class="actions">
              <button @click="runNow(t)">立即执行</button>
              <button class="danger" @click="remove(t)">删除</button>
            </div>
          </td>
        </tr>
      </tbody>
    </table>
    <div v-else class="empty">
      还没有任务。STRM 任务会把网盘里的影片（含 .cas）在本地输出目录生成 .strm，供 Emby / Jellyfin 播放。
    </div>
  </div>

  <p class="muted mono" style="margin-top: 14px" v-if="streamKey">
    STRM 播放密钥：{{ streamKey }}（已写进生成的链接里，重启不变）
  </p>

  <div v-if="showForm" class="modal-mask" @click.self="showForm = false">
    <div class="card modal">
      <div class="page-head" style="margin-bottom: 10px">
        <h2 style="font-size: 16px">新建 STRM 任务</h2>
        <div class="spacer" />
        <button class="ghost" @click="showForm = false">关闭</button>
      </div>

      <label class="field">
        <span>任务名 <em>*</em></span>
        <input v-model="form.name" placeholder="例如 移动云盘电影" />
      </label>

      <div class="row">
        <label class="field">
          <span>挂载点</span>
          <select v-model="form.mount">
            <option v-for="m in mounts" :key="m.name" :value="m.name">{{ m.name }}（{{ m.type }}）</option>
          </select>
        </label>
        <label class="field">
          <span>源目录（挂载内路径）</span>
          <input v-model="form.dir" placeholder="/" />
        </label>
      </div>

      <label class="field">
        <span>输出目录（本机绝对路径） <em>*</em></span>
        <div style="display: flex; gap: 8px">
          <input v-model="form.target" placeholder="例如 /vol1/1000/Emby/strm/movies 或 D:/strm/movies" />
          <button :disabled="testing" @click="testTarget" style="white-space: nowrap">
            {{ testing ? '检测中…' : '测试目录' }}
          </button>
        </div>
        <small class="muted">
          容器部署时填容器内路径（如 <code>/app/strm/movies</code>），对应宿主机映射出来的目录
        </small>
      </label>

      <div class="row">
        <label class="field">
          <span>链接方式</span>
          <select v-model="form.mode">
            <option value="proxy">代理播放（兼容性最好）</option>
            <option value="link">网盘直链（省服务器流量，需驱动支持）</option>
          </select>
        </label>
        <label class="field">
          <span>执行间隔</span>
          <input v-model="form.interval" placeholder="6h / 30m / 1d / manual" />
        </label>
      </div>

      <label class="field" style="display: flex; align-items: center; gap: 8px">
        <input v-model="form.enabled" type="checkbox" style="width: auto" />
        <span style="margin: 0">创建后立即启用</span>
      </label>

      <button class="primary" :disabled="busy" @click="submit">
        {{ busy ? '创建中…' : '创建任务' }}
      </button>
    </div>
  </div>
</template>
