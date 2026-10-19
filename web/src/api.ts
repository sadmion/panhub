// 极简 API 客户端：token 放 localStorage，所有请求自动带 Authorization。
const TOKEN_KEY = 'panhub_token'

export const getToken = () => localStorage.getItem(TOKEN_KEY)
export const setToken = (t: string) => localStorage.setItem(TOKEN_KEY, t)
export const clearToken = () => localStorage.removeItem(TOKEN_KEY)

async function request(path: string, init: RequestInit = {}): Promise<any> {
  const headers: Record<string, string> = { ...(init.headers as Record<string, string>) }
  const tok = getToken()
  if (tok) headers['Authorization'] = `Bearer ${tok}`
  if (init.body && !(init.body instanceof FormData)) {
    headers['Content-Type'] = 'application/json'
  }
  const res = await fetch('/api' + path, { ...init, headers })
  const text = await res.text()
  let data: any = {}
  try {
    data = text ? JSON.parse(text) : {}
  } catch {
    data = { error: text }
  }
  if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`)
  return data
}

export const api = {
  get: (p: string) => request(p),
  post: (p: string, body?: any) =>
    request(p, { method: 'POST', body: body === undefined ? undefined : JSON.stringify(body) }),
  put: (p: string, body?: any) =>
    request(p, { method: 'PUT', body: body === undefined ? undefined : JSON.stringify(body) }),
  del: (p: string) => request(p, { method: 'DELETE' }),
  upload: (path: string, file: File) => {
    const fd = new FormData()
    fd.append('file', file)
    return request(`/fs/upload?path=${encodeURIComponent(path)}`, { method: 'POST', body: fd })
  },
}

// 直接下载 / 播放链接（浏览器会带不上 header，所以用 query token）
export function fileURL(path: string, mode: 'download' | 'stream' = 'download') {
  const tok = getToken() || ''
  const base = mode === 'download' ? '/api/fs/download' : '/api/stream'
  return `${base}?path=${encodeURIComponent(path)}&token=${encodeURIComponent(tok)}`
}

export function fmtSize(n: number) {
  if (!n) return '-'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

export function fmtTime(s?: string) {
  if (!s) return '-'
  const d = new Date(s)
  if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '-'
  const p = (x: number) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}
