import { useEffect, useState } from 'react'
import { Button } from 'antd'
import type { PurgeProgress, PurgeProgressReader, PurgeReceipt } from './api/drive'
import { apiErrorMessage } from './api/http'

type ProgressState =
  | { status: 'loading' }
  | { status: 'ready'; progress: PurgeProgress }
  | { status: 'error'; message: string }

export default function PurgeProgressPanel({ name, receipt, readProgress }: {
  name: string
  receipt: PurgeReceipt
  readProgress: PurgeProgressReader
}) {
  const [state, setState] = useState<ProgressState>({ status: 'loading' })
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    const controller = new AbortController()

    async function loadProgress() {
      try {
        // 1. 受理后读取一次，手动刷新继续使用同一回执。
        const progress = await readProgress(receipt, controller.signal)
        // 2. 只展示当前查询结果，忽略离开页面后返回的数据。
        if (!controller.signal.aborted) setState({ status: 'ready', progress })
      } catch (error) {
        // 3. 读取失败时保留受理提示，允许用户手动重试。
        if (!controller.signal.aborted) setState({ status: 'error', message: apiErrorMessage(error) })
      }
    }

    void loadProgress()
    return () => controller.abort()
  }, [attempt, readProgress, receipt])

  function refreshProgress() {
    setState({ status: 'loading' })
    setAttempt(previous => previous + 1)
  }

  return (
    <section aria-label="永久清理进度">
      <p>「{name}」的永久清理已受理。</p>
      {state.status === 'loading' && <p role="status">正在读取清理进度…</p>}
      {state.status === 'ready' && (
        <p role="status">{state.progress.status === 'PURGED' ? '清理完成' : '清理中'}，剩余 {state.progress.remainingCount} 项。</p>
      )}
      {state.status === 'error' && <p className="auth-error" role="alert">无法读取清理进度。{state.message}</p>}
      <Button onClick={refreshProgress} disabled={state.status === 'loading'}>刷新清理进度</Button>
    </section>
  )
}
