import { useEffect, useState } from 'react'
import { Button, Modal } from 'antd'
import type { PurgePreviewReader, PurgePreviewScope, TrashEntrySummary } from './api/drive'
import { apiErrorMessage } from './api/http'

type PreviewState =
  | { status: 'loading' }
  | { status: 'ready'; scope: PurgePreviewScope }
  | { status: 'error'; message: string }

export default function PurgePreviewDialog({ target, originalLocation, readPreview, onClose }: {
  target: TrashEntrySummary
  originalLocation: string
  readPreview: PurgePreviewReader
  onClose: () => void
}) {
  const [preview, setPreview] = useState<PreviewState>({ status: 'loading' })
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    const controller = new AbortController()

    async function loadPreview() {
      try {
        // 1. 只读查询选中条目的永久清理预检。
        const scope = await readPreview(target.id, controller.signal)
        // 2. 保留服务端汇总的展示范围。
        if (!controller.signal.aborted) setPreview({ status: 'ready', scope })
      } catch (error) {
        // 3. 显示失败原因，忽略窗口关闭后的结果。
        if (!controller.signal.aborted) setPreview({ status: 'error', message: apiErrorMessage(error) })
      }
    }

    void loadPreview()
    return () => controller.abort()
  }, [attempt, readPreview, target.id])

  function retryPreview() {
    setPreview({ status: 'loading' })
    setAttempt(previous => previous + 1)
  }

  return (
    <Modal title="永久清理预检" open onCancel={onClose}
      footer={<Button onClick={onClose}>关闭预检</Button>} width={440}>
      <p><strong>只读预检，未执行清理。</strong></p>
      <dl className="auth-profile">
        <div><dt>目标条目</dt><dd>{target.name}</dd></div>
        <div><dt>原位置</dt><dd>{originalLocation}</dd></div>
        {preview.status === 'ready' && (
          <>
            <div><dt>文件数</dt><dd>{preview.scope.fileCount}</dd></div>
            <div><dt>目录数</dt><dd>{preview.scope.directoryCount}</dd></div>
            <div><dt>预计释放的逻辑空间</dt><dd>{preview.scope.logicalBytes} 字节</dd></div>
          </>
        )}
      </dl>
      {preview.status === 'loading' && <p role="status">正在读取预检范围…</p>}
      {preview.status === 'error' && (
        <>
          <p className="auth-error" role="alert">无法读取清理预检。{preview.message}</p>
          <Button onClick={retryPreview}>重试预检</Button>
        </>
      )}
    </Modal>
  )
}
