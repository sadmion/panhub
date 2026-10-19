<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'

const emit = defineEmits<{ (e: 'changed'): void }>()

const drivers = ref<any[]>([])
const mounts = ref<any[]>([])
const error = ref('')
const notice = ref('')
const busy = ref(false)
const showForm = ref(false)

const form = reactive<any>({ name: '', type: '', read_only: false, config: {} as Record<string, any> })

const currentDriver = computed(() => drivers.value.find((d) => d.type === form.type))

// ---- 登录 ----
const loginMode = ref('')
const loginValues = reactive<Record<string, any>>({})
const loginBusy = ref(false)
const loginMsg = ref('')
const loginProfile = ref<any>(null)

const loginModes = computed<string[]>(() => {
  const m = currentDriver.value?.login_modes
  return Array.isArray(m) && m.length ? m : ['password']
})
const loginFields = computed<any[]>(() => {
  const f = currentDriver.value?.login_fields?.[loginMode.value]
  return Array.isArray(f) ? f : []
})

const modeLabels: Record<string, string> = {
  password: '账号密码',
  authorization: '粘贴授权',
  cookies: 'Cookie 登录',
}

async function load() {
  error.value = ''
  try {
    const [d, m] = await Promise.all([api.get('/drivers'), api.get('/mounts')])
    drivers.value = d.drivers || []
    mounts.value = m.mounts || []
    if (!form.type && drivers.value.length) selectType(drivers.value[0].type)
  } catch (e: any) {
    error.value = e.message
  }
}

function selectType(t: string) {
  form.type = t
  form.config = {}
  const d = drivers.value.find((x) => x.type === t)
  for (const f of d?.fields || []) form.config[f.key] = f.type === 'bool' ? false : ''
  resetLogin()
}

function resetLogin() {
  loginMsg.value = ''
  loginProfile.value = null
  for (const k of Object.keys(loginValues)) delete loginValues[k]
  const modes = currentDriver.value?.login_modes || ['password']
  loginMode.value = modes[0]
  for (const f of currentDriver.value?.login_fields?.[loginMode.value] || []) loginValues[f.key] = ''
}

function pickMode(m: string) {
  loginMode.value = m
  loginProfile.value = null
  loginMsg.value = ''
  for (const k of Object.keys(loginValues)) delete loginValues[k]
  for (const f of currentDriver.value?.login_fields?.[m] || []) loginValues[f.key] = ''
}

async function doLogin() {
  loginBusy.value = true
  loginMsg.value = ''
  loginProfile.value = null
  error.value = ''
  try {
    const res = await api.post(`/drivers/${form.type}/login`, {
      mode: loginMode.value,
      fields: loginValues,
    })
    // 把会话字段合并进挂载配置（authorization / account / mail_cookies…）
    Object.assign(form.config, res.session || {})
    loginProfile.value = res.profile || {}
    loginMsg.value = res.message || '登录成功，已自动填入授权'
  } catch (e: any) {
    loginMsg.value = '登录失败：' + e.message
  } finally {
    loginBusy.value = false
  }
}

function openForm() {
  form.name = ''
  form.read_only = false
  if (drivers.value.length) selectType(drivers.value[0].type)
  showForm.value = true
  notice.value = ''
}

async function submit() {
  busy.value = true
  error.value = ''
  try {
    await api.post('/mounts', {
      name: form.name,
      type: form.type,
      read_only: form.read_only,
      config: form.config,
    })
    showForm.value = false
    notice.value = '挂载已添加'
    await load()
    emit('changed')
  } catch (e: any) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}

async function remove(name: string) {
  if (!confirm(`删除挂载「${name}」？\n只移除挂载，不会删除网盘里的文件。`)) return
  error.value = ''
  try {
    await api.del(`/mounts/${encodeURIComponent(name)}`)
    await load()
    emit('changed')
  } catch (e: any) {
    error.value = e.message
  }
}

onMounted(load)
</script>

<template>
  <div class="page-head">
    <h2>挂载管理</h2>
    <div class="spacer" />
    <button class="primary" @click="openForm">＋ 新建挂载</button>
  </div>

  <div v-if="error" class="alert error">{{ error }}</div>
  <div v-if="notice" class="alert ok">{{ notice }}</div>

  <div class="card" style="padding: 0">
    <table v-if="mounts.length">
      <thead>
        <tr>
          <th>名称</th>
          <th>驱动</th>
          <th>能力</th>
          <th>模式</th>
          <th style="width: 90px"></th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="m in mounts" :key="m.name">
          <td><strong>{{ m.name }}</strong></td>
          <td class="muted">{{ m.type }}</td>
          <td>
            <span class="tag" :class="{ on: m.caps.read }">读</span>
            <span class="tag" :class="{ on: m.caps.write }">写</span>
            <span class="tag" :class="{ on: m.caps.link }">直链</span>
          </td>
          <td>
            <span class="tag" :class="{ on: !m.read_only }">{{ m.read_only ? '只读' : '可写' }}</span>
          </td>
          <td class="actions">
            <button class="danger" @click="remove(m.name)">删除</button>
          </td>
        </tr>
      </tbody>
    </table>
    <div v-else class="empty">
      还没有挂载点。点右上角「新建挂载」添加本地目录或 WebDAV。
    </div>
  </div>

  <div v-if="showForm" class="modal-mask" @click.self="showForm = false">
    <div class="card modal">
      <div class="page-head" style="margin-bottom: 10px">
        <h2 style="font-size: 16px">新建挂载</h2>
        <div class="spacer" />
        <button class="ghost" @click="showForm = false">关闭</button>
      </div>

      <div class="row">
        <label class="field">
          <span>挂载名 <em>*</em></span>
          <input v-model="form.name" placeholder="例如 local / mydav / yidong" />
        </label>
        <label class="field">
          <span>驱动 <em>*</em></span>
          <select :value="form.type" @change="selectType(($event.target as HTMLSelectElement).value)">
            <option v-for="d in drivers" :key="d.type" :value="d.type">{{ d.name }}</option>
          </select>
        </label>
      </div>

      <!-- 需要登录的驱动：先换授权，再填挂载 -->
      <div v-if="currentDriver?.login" class="login-box">
        <div class="login-title">
          <strong>① 登录获取授权</strong>
          <span class="muted">（登录成功后会自动填到下面的配置里）</span>
        </div>

        <div v-if="loginModes.length > 1" class="toolbar" style="margin-bottom: 10px">
          <button
            v-for="m in loginModes"
            :key="m"
            :class="{ primary: loginMode === m }"
            @click="pickMode(m)"
          >
            {{ modeLabels[m] || m }}
          </button>
        </div>

        <label class="field" v-for="f in loginFields" :key="f.key">
          <span>{{ f.label }} <em v-if="f.required">*</em></span>
          <input
            v-model="loginValues[f.key]"
            :type="f.type === 'password' ? 'password' : 'text'"
            :placeholder="f.help || ''"
          />
          <small v-if="f.help" class="muted">{{ f.help }}</small>
        </label>

        <button class="primary" :disabled="loginBusy" @click="doLogin">
          {{ loginBusy ? '登录中…' : '登录' }}
        </button>

        <div v-if="loginMsg" class="alert" :class="loginProfile ? 'ok' : 'error'" style="margin-top: 10px">
          {{ loginMsg }}
        </div>
        <div v-if="loginProfile" class="mono muted" style="font-size: 12px">
          账号：{{ loginProfile.account || '-' }}
          <template v-if="loginProfile.expires_at"> · 到期：{{ loginProfile.expires_at }}</template>
          <template v-if="loginProfile.root_items !== undefined"> · 根目录 {{ loginProfile.root_items }} 项</template>
        </div>
      </div>

      <label class="field" v-for="f in currentDriver?.fields || []" :key="f.key">
        <span>{{ f.label }} <em v-if="f.required && !currentDriver?.login">*</em></span>
        <input
          v-if="f.type !== 'bool'"
          v-model="form.config[f.key]"
          :type="f.type === 'password' ? 'password' : 'text'"
          :placeholder="f.placeholder || ''"
        />
        <input v-else v-model="form.config[f.key]" type="checkbox" style="width: auto" />
        <small v-if="f.help" class="muted">{{ f.help }}</small>
      </label>

      <label class="field" style="display: flex; align-items: center; gap: 8px">
        <input v-model="form.read_only" type="checkbox" style="width: auto" />
        <span style="margin: 0">只读挂载（禁止写入/删除，推荐给网盘）</span>
      </label>

      <button class="primary" :disabled="busy" @click="submit">
        {{ busy ? '创建中…' : '创建并测试连接' }}
      </button>
    </div>
  </div>
</template>

<style scoped>
.login-box {
  border: 1px dashed var(--line);
  border-radius: var(--radius);
  padding: 12px 14px;
  margin-bottom: 16px;
  background: rgba(108, 99, 255, 0.06);
}
.login-title {
  margin-bottom: 10px;
  font-size: 13px;
}
</style>
