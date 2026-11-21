import { useEffect, useRef, useState } from 'react'
import { Button } from 'antd'
import type { FileAccessGrant } from './api/download'
import { apiErrorMessage } from './api/http'
import { readPlainTextPreview } from './api/plainTextPreview'

type TextPreviewState = { phase: 'idle' | 'loading' } | { phase: 'ready'; text: string } | { phase: 'error'; message: string }

export default function PlainTextPreview({ grant, onRefresh }: {
  grant: FileAccessGrant
  onRefresh: () => void
}) {
  const [state, setState] = useState<TextPreviewState>({ phase: 'idle' })
  const activeRead = useRef<AbortController | null>(null)

  useEffect(() => () => activeRead.current?.abort(), [])

  async function showText() {
    // 1. 仅响应用户点击，申请已过期时提供重新获取入口。
    if (activeRead.current) return
    if (grant.expiresAt <= Date.now()) {
      setState({ phase: 'error', message: '预览链接已过期。' })
      return
    }

    // 2. 用当前链接读取正文，关闭窗口时取消在途请求。
    const controller = new AbortController()
    activeRead.current = controller
    setState({ phase: 'loading' })
    try {
      const text = await readPlainTextPreview(grant, controller.signal)
      if (!controller.signal.aborted) setState({ phase: 'ready', text })
    } catch (error) {
      if (!controller.signal.aborted) setState({ phase: 'error', message: `文本加载失败。${apiErrorMessage(error)}` })
    } finally {
      activeRead.current = null
    }
  }

  return (
    <div className="entry-name-form">
      {state.phase === 'idle' && <Button onClick={() => void showText()}>查看文本</Button>}
      {state.phase === 'loading' && <p role="status">正在加载文本…</p>}
      {state.phase === 'ready' && (state.text ? (
        // 3. 用文本节点保留换行，交给 React 转义正文。
        <pre className="plain-text-preview" aria-label="纯文本正文" tabIndex={0}>{state.text}</pre>
      ) : <p role="status">此文件为空。</p>)}
      {state.phase === 'error' && (
        <>
          <p role="alert" className="auth-error">{state.message}</p>
          <Button onClick={onRefresh}>重新获取预览链接</Button>
        </>
      )}
    </div>
  )
}
