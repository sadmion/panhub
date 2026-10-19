<script setup lang="ts">
import { ref } from 'vue'
import { api, setToken } from '../api'

const emit = defineEmits<{ (e: 'ok'): void }>()

const password = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  if (!password.value) {
    error.value = '请输入密码'
    return
  }
  busy.value = true
  error.value = ''
  try {
    const r = await api.post('/auth/login', { password: password.value })
    setToken(r.token)
    emit('ok')
  } catch (e: any) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="login-wrap">
    <div class="card login-card">
      <h1>panhub</h1>
      <p class="muted">多网盘聚合挂载</p>
      <div v-if="error" class="alert error">{{ error }}</div>
      <label class="field">
        <span>管理员密码</span>
        <input v-model="password" type="password" placeholder="默认 admin" @keyup.enter="submit" />
      </label>
      <button class="primary" style="width: 100%" :disabled="busy" @click="submit">
        {{ busy ? '登录中…' : '登录' }}
      </button>
    </div>
  </div>
</template>
