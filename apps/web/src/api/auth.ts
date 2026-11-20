export interface CurrentUserProfile {
  user: { id: string; username: string; display_name: string }
  root_entry_id: string
  quota_bytes: number
  used_bytes: number
  reserved_bytes: number
}

export class AuthApiError extends Error {
  constructor(readonly status: number, message: string) {
    super(message)
  }
}

const installationKey = 'cloudnest.web.installation_id'
const installationPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/

function browserInstallationId(): string {
  const storedId = localStorage.getItem(installationKey)
  if (storedId && installationPattern.test(storedId)) return storedId
  const installationId = crypto.randomUUID()
  localStorage.setItem(installationKey, installationId)
  return installationId
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

function isByteCount(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
}

async function requestData(path: string, init: RequestInit): Promise<unknown> {
  const response = await fetch(path, { ...init, credentials: 'same-origin', cache: 'no-store' })
  const payload: unknown = await response.json().catch(() => null)
  if (!response.ok) {
    const error = isRecord(payload) && isRecord(payload.error) ? payload.error : null
    const message = error && typeof error.message === 'string' ? error.message : '服务暂时无法完成请求，请重试。'
    throw new AuthApiError(response.status, message)
  }
  if (!isRecord(payload) || !isRecord(payload.data)) {
    throw new AuthApiError(response.status, '服务返回的数据格式有误。')
  }
  return payload.data
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
  if (!isRecord(data) || typeof data.access_token !== 'string' || !data.access_token) {
    throw new AuthApiError(200, '登录响应缺少访问凭证。')
  }
  return data.access_token
}

export async function getCurrentUser(accessToken: string): Promise<CurrentUserProfile> {
  // 1. 用 Access Token 读取当前会话对应的资料。
  const data = await requestData('/api/v1/me', {
    headers: { Authorization: `Bearer ${accessToken}` },
  })

  // 2. 核对资料形状，保留服务端的字符串 ID 和字节计数。
  if (!isRecord(data) || !isRecord(data.user) ||
      typeof data.user.id !== 'string' || typeof data.user.username !== 'string' ||
      typeof data.user.display_name !== 'string' || typeof data.root_entry_id !== 'string' ||
      !isByteCount(data.quota_bytes) || !isByteCount(data.used_bytes) || !isByteCount(data.reserved_bytes)) {
    throw new AuthApiError(200, '服务返回的本人资料格式有误。')
  }
  return {
    user: { id: data.user.id, username: data.user.username, display_name: data.user.display_name },
    root_entry_id: data.root_entry_id,
    quota_bytes: data.quota_bytes,
    used_bytes: data.used_bytes,
    reserved_bytes: data.reserved_bytes,
  }
}
