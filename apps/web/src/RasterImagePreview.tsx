import { useState } from 'react'
import { Button } from 'antd'
import type { FileAccessGrant } from './api/download'

type ImagePreviewState = 'idle' | 'loading' | 'ready' | 'expired' | 'error'

export default function RasterImagePreview({ filename, grant, onRefresh }: {
  filename: string
  grant: FileAccessGrant
  onRefresh: () => void
}) {
  const [state, setState] = useState<ImagePreviewState>('idle')

  function showImage() {
    // 1. 点击查看时复查链接有效期，避免向存储发送过期请求。
    if (grant.expiresAt <= Date.now()) {
      setState('expired')
      return
    }

    // 2. 进入加载状态时才让浏览器读取签名图片地址。
    setState('loading')
  }

  return (
    <div className="entry-name-form">
      {state === 'idle' && <Button onClick={showImage}>查看图片</Button>}
      {state === 'loading' && <p role="status">正在加载图片…</p>}
      {(state === 'loading' || state === 'ready') && (
        // 3. 浏览器解码成功后显示图片，读取失败交给重新授权入口。
        <img src={grant.url} alt={filename} referrerPolicy="no-referrer" className="raster-preview-image"
          hidden={state === 'loading'} onLoad={() => setState('ready')} onError={() => setState('error')} />
      )}
      {(state === 'expired' || state === 'error') && (
        <>
          <p role="alert" className="auth-error">{state === 'expired' ? '预览链接已过期。' : '图片加载失败。'}</p>
          <Button onClick={onRefresh}>重新获取预览链接</Button>
        </>
      )}
    </div>
  )
}
