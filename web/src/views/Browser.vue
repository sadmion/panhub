<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, fileURL, fmtSize, fmtTime } from '../api'

const path = ref('/')
const entries = ref<any[]>([])
const error = ref('')
const loading = ref(false)
const uploading = ref('')

const crumbs = computed(() => {
  const parts = path.value.split('/').filter(Boolean)
  const out = [{ name: '根目录', path: '/' }]
  let acc = ''
  for (const p of parts) {
    acc += '/' + p
    out.push({ name: p, path: acc })
  }
  return out
})

function join(dir: string, name: string) {
  return dir === '/' ? '/' + name : dir + '/' + name
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const r = await api.get(`/fs/list?path=${encodeURIComponent(path.value)}`)
    entries.value = r.entries || []
  } catch (e: any) {
    error.value = e.message
    entries.value = []
  } finally {
    loading.value = false
  }
}

function open(e: any) {
  if (!e.is_dir) return
  path.value = e.path
  load()
}

function goto(p: string) {
  path.value = p
  load()
}

function up() {
  const i = path.value.lastIndexOf('/')
  goto(i <= 0 ? '/' : path.value.slice(0, i))
}

async function mkdir() {
  const name = prompt('新文件夹名称')
  if (!name) return
  try {
    await api.post('/fs/mkdir', { path: join(path.value, name) })
    await load()
  } catch (e: any) {
    error.value = e.message
  }
}

async function onPick(ev: Event) {
  const input = ev.target as HTMLInputElement
  const files = Array.from(input.files || [])
  if (!files.length) return
  error.value = ''
  for (const f of files) {
    uploading.value = f.name
    try {
      await api.upload(path.value, f)
    } catch (e: any) {
      error.value = `${f.name}: ${e.message}`
      break
    }
  }
  uploading.value = ''
  input.value = ''
  await load()
}

async function remove(e: any) {
  if (!confirm(`删除「${e.name}」？此操作不可撤销。`)) return
  try {
    await api.post('/fs/remove', { path: e.path, recursive: e.is_dir })
    await load()
  } catch (err: any) {
    error.value = err.message
  }
}

async function copyLink(e: any) {
  const u = location.origin + fileURL(e.path, 'download')
  try {
    await navigator.clipboard.writeText(u)
    error.value = ''
    alert('下载链接已复制（有效期跟随登录会话）')
  } catch {
    prompt('复制下面的链接：', u)
  }
}

onMounted(load)
</script>

<template>
  <div class="page-head">
    <h2>文件浏览</h2>
    <div class="spacer" />
    <button @click="load" :disabled="loading">刷新</button>
    <button @click="up">上一级</button>
    <button @click="mkdir">新建文件夹</button>
    <label class="btn" style="cursor: pointer; display: inline-flex; align-items: center">
      上传文件
      <input type="file" multiple style="display: none" @change="onPick" />
    </label>
  </div>

  <div class="crumbs" style="margin-bottom: 12px">
    <template v-for="(c, i) in crumbs" :key="c.path">
      <span v-if="i" class="sep">/</span>
      <button @click="goto(c.path)">{{ c.name }}</button>
    </template>
  </div>

  <div v-if="error" class="alert error">{{ error }}</div>
  <div v-if="uploading" class="alert ok">正在上传：{{ uploading }}</div>

  <div class="card" style="padding: 0">
    <table v-if="entries.length">
      <thead>
        <tr>
          <th class="icon-cell"></th>
          <th>名称</th>
          <th style="width: 110px">大小</th>
          <th style="width: 170px">修改时间</th>
          <th style="width: 210px"></th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="e in entries" :key="e.path">
          <td class="icon-cell">{{ e.is_dir ? '📁' : '📄' }}</td>
          <td>
            <span class="file-name" :class="{ dir: e.is_dir }" @click="open(e)">{{ e.name }}</span>
          </td>
          <td class="muted">{{ e.is_dir ? '-' : fmtSize(e.size) }}</td>
          <td class="muted">{{ fmtTime(e.mod_time) }}</td>
          <td>
            <div class="actions">
              <a v-if="!e.is_dir" class="btn" :href="fileURL(e.path, 'download')" target="_blank">下载</a>
              <button v-if="!e.is_dir" @click="copyLink(e)">链接</button>
              <button class="danger" @click="remove(e)">删除</button>
            </div>
          </td>
        </tr>
      </tbody>
    </table>
    <div v-else class="empty">
      {{ loading ? '加载中…' : '这个目录是空的。根目录里会列出所有挂载点。' }}
    </div>
  </div>
</template>
