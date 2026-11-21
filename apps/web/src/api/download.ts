import { ApiError, requestData } from './http'

export type FileAccessPurpose = 'download' | 'preview'

const rasterPreviewMimeTypes = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'] as const
type RasterPreviewMimeType = typeof rasterPreviewMimeTypes[number]
type PreviewMimeType = RasterPreviewMimeType | 'text/plain'

export interface FileAccessGrant {
  url: string
  expiresAt: number
  previewMimeType: PreviewMimeType | null
}

export type FileAccessAuthorizer = (entryId: string, expectedRevision: string, purpose: FileAccessPurpose, signal: AbortSignal) => Promise<FileAccessGrant>

export function isRasterPreviewMimeType(value: unknown): value is RasterPreviewMimeType {
  return rasterPreviewMimeTypes.some(mimeType => mimeType === value)
}

function isSignedAccessUrl(value: unknown): value is string {
  if (typeof value !== 'string' || !/^https?:\/\/[^#\\\s\u0000-\u001f\u007f]+$/.test(value)) return false
  try {
    const parsed = new URL(value)
    return !parsed.username && !parsed.password
  } catch {
    return false
  }
}

export async function createFileAccessGrant(accessToken: string, entryId: string, expectedRevision: string, purpose: FileAccessPurpose, signal: AbortSignal): Promise<FileAccessGrant> {
  // 1. 按当前文件版本和用户选择的用途，调用现有同源授权入口。
  const data = await requestData(`/api/v1/entries/${encodeURIComponent(entryId)}/download`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${accessToken}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ expected_revision: expectedRevision, purpose }),
    redirect: 'error',
    signal,
  }, 200)

  // 2. 核对授权版本、交付方式和预览 MIME，原样保留签名 URL。
  const preview = purpose === 'preview'
  const purposeLabel = preview ? '预览' : '下载'
  const dispositionPrefix = preview ? 'inline;' : 'attachment;'
  const expiresAt = typeof data.expires_at === 'string' ? Date.parse(data.expires_at) : NaN
  const mimeType = data.mime_type
  const previewMimeType = preview && (isRasterPreviewMimeType(mimeType) || mimeType === 'text/plain') ? mimeType : null
  if (data.revision !== expectedRevision || !isSignedAccessUrl(data.url) || !Number.isFinite(expiresAt) ||
      typeof data.disposition !== 'string' || !data.disposition.startsWith(dispositionPrefix) ||
      (preview && !previewMimeType)) {
    throw new ApiError(200, `服务返回的${purposeLabel}授权有误。`)
  }
  if (expiresAt <= Date.now()) throw new ApiError(200, `${purposeLabel}链接已过期，请重新获取。`)
  return { url: data.url, expiresAt, previewMimeType }
}
