import { ApiError } from './http'
import { maxPlainTextPreviewBytes } from './download'
import type { FileAccessGrant } from './download'

export async function readPlainTextPreview(grant: FileAccessGrant, signal: AbortSignal): Promise<string> {
  // 1. 使用短期签名读取正文，不把网盘鉴权信息传给存储。
  if (grant.previewMimeType !== 'text/plain' || grant.sizeBytes > maxPlainTextPreviewBytes) {
    throw new ApiError(0, '此文件不符合纯文本预览条件。')
  }
  const response = await fetch(grant.url, {
    credentials: 'omit', redirect: 'error', referrerPolicy: 'no-referrer', cache: 'no-store', signal,
  })
  const reader = response.body?.getReader()
  try {
    // 2. 检查响应类型，按授权字节数分配最多 2 MiB 的缓冲区。
    if (response.status !== 200) throw new ApiError(response.status, '存储未能返回文本正文。')
    const contentType = response.headers.get('Content-Type')?.split(';', 1)[0].trim().toLowerCase()
    if (contentType !== 'text/plain') throw new ApiError(200, '返回的内容类型不是纯文本。')

    const bytes = new Uint8Array(grant.sizeBytes)

    // 3. 逐段读取，验证完整正文后才交给页面以文本形式展示。
    let received = 0
    if (reader) {
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        if (received + value.byteLength > bytes.byteLength) {
          throw new ApiError(200, '文本字节数与授权不一致。')
        }
        bytes.set(value, received)
        received += value.byteLength
      }
    }
    if (received !== bytes.byteLength) throw new ApiError(200, '文本字节数与授权不一致。')
    return new TextDecoder().decode(bytes)
  } finally {
    if (reader) {
      await reader.cancel().catch(() => {})
      reader.releaseLock()
    }
  }
}
