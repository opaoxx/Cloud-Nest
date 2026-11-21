import { ApiError, requestData } from './http'

export interface DownloadGrant {
  url: string
  expiresAt: number
}

export type DownloadLinkAuthorizer = (entryId: string, expectedRevision: string, signal: AbortSignal) => Promise<DownloadGrant>

function isDownloadUrl(value: unknown): value is string {
  if (typeof value !== 'string' || !/^https?:\/\/[^#\\\s\u0000-\u001f\u007f]+$/.test(value)) return false
  try {
    const parsed = new URL(value)
    return !parsed.username && !parsed.password
  } catch {
    return false
  }
}

export async function createDownloadGrant(accessToken: string, entryId: string, expectedRevision: string, signal: AbortSignal): Promise<DownloadGrant> {
  // 1. 按当前文件版本申请下载授权，使用现有同源认证请求。
  const data = await requestData(`/api/v1/entries/${encodeURIComponent(entryId)}/download`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${accessToken}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ expected_revision: expectedRevision, purpose: 'download' }),
    redirect: 'error',
    signal,
  }, 200)

  // 2. 核对授权版本、附件用途、签名地址和有效期，原样保留签名 URL。
  const expiresAt = typeof data.expires_at === 'string' ? Date.parse(data.expires_at) : NaN
  if (data.revision !== expectedRevision || !isDownloadUrl(data.url) || !Number.isFinite(expiresAt) ||
      typeof data.disposition !== 'string' || !data.disposition.startsWith('attachment;')) {
    throw new ApiError(200, '服务返回的下载授权有误。')
  }
  if (expiresAt <= Date.now()) throw new ApiError(200, '下载链接已过期，请重新获取。')
  return { url: data.url, expiresAt }
}
