import { ApiError, isByteCount, isRecord, requestData } from './http'

export interface CurrentUserProfile {
  user: { id: string; username: string; display_name: string }
  root_entry_id: string
  quota_bytes: number
  used_bytes: number
  reserved_bytes: number
}

const installationKey = 'cloudnest.web.installation_id'
const installationPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/
let pendingRefresh: Promise<string> | undefined

function browserInstallationId(): string {
  const storedId = localStorage.getItem(installationKey)
  if (storedId && installationPattern.test(storedId)) return storedId
  const installationId = crypto.randomUUID()
  localStorage.setItem(installationKey, installationId)
  return installationId
}

function readAccessToken(data: Record<string, unknown>): string {
  if (typeof data.access_token !== 'string' || !data.access_token) {
    throw new ApiError(200, '服务响应缺少访问凭证。')
  }
  return data.access_token
}

export async function login(username: string, password: string): Promise<string> {
  // 1. 复用浏览器安装标识，提交 Web 登录契约。
  const data = await requestData('/api/v1/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-CloudNest-Client': 'web' },
    body: JSON.stringify({
      username,
      password,
      installation_id: browserInstallationId(),
      device_name: 'CloudNest 浏览器',
      platform: 'WEB',
      client_type: 'web',
    }),
  })

  // 2. 只取 Access Token，浏览器自行管理 HttpOnly 刷新 Cookie。
  return readAccessToken(data)
}

export function refreshWebSession(): Promise<string> {
  // 1. 同一页面内共享正在进行的刷新，避免重复轮换 Cookie。
  if (pendingRefresh) return pendingRefresh

  // 2. 发送 Web 刷新请求，浏览器自动携带 HttpOnly Cookie。
  const response = requestData('/api/v1/auth/refresh', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-CloudNest-Client': 'web' },
    body: JSON.stringify({ client_type: 'web' }),
  })

  // 3. 交付新的 Access Token，刷新结束后释放共享请求。
  pendingRefresh = response.then(readAccessToken).finally(() => {
    pendingRefresh = undefined
  })
  return pendingRefresh
}

export async function getCurrentUser(accessToken: string): Promise<CurrentUserProfile> {
  // 1. 用 Access Token 读取当前会话对应的资料。
  const data = await requestData('/api/v1/me', {
    headers: { Authorization: `Bearer ${accessToken}` },
  })

  // 2. 核对资料形状，保留服务端的字符串 ID 和字节计数。
  if (!isRecord(data.user) ||
      typeof data.user.id !== 'string' || typeof data.user.username !== 'string' ||
      typeof data.user.display_name !== 'string' || typeof data.root_entry_id !== 'string' ||
      !isByteCount(data.quota_bytes) || !isByteCount(data.used_bytes) || !isByteCount(data.reserved_bytes)) {
    throw new ApiError(200, '服务返回的本人资料格式有误。')
  }
  return {
    user: { id: data.user.id, username: data.user.username, display_name: data.user.display_name },
    root_entry_id: data.root_entry_id,
    quota_bytes: data.quota_bytes,
    used_bytes: data.used_bytes,
    reserved_bytes: data.reserved_bytes,
  }
}
