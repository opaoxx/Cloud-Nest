import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { Button, Modal } from 'antd'
import type { PurgeConfirmer, PurgePreview, PurgePreviewReader, PurgeReceipt, TrashEntrySummary } from './api/drive'
import { ApiError, apiErrorMessage } from './api/http'

type PreviewState =
  | { status: 'loading' }
  | { status: 'ready'; scope: PurgePreview }
  | { status: 'error'; message: string; refreshList?: boolean }

const expiredMessage = '清理预检已过期，请重新预检。'
const changedMessage = '条目或清理范围已变化，请重新预检。'

export default function PurgePreviewDialog({ target, originalLocation, readPreview, confirmPurge, onAccepted, onClose, onRefreshTrash }: {
  target: TrashEntrySummary
  originalLocation: string
  readPreview: PurgePreviewReader
  confirmPurge: PurgeConfirmer
  onAccepted: (receipt: PurgeReceipt) => void
  onClose: () => void
  onRefreshTrash: () => void
}) {
  const [preview, setPreview] = useState<PreviewState>({ status: 'loading' })
  const [attempt, setAttempt] = useState(0)
  const [confirming, setConfirming] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const cancelButton = useRef<HTMLButtonElement | HTMLAnchorElement>(null)
  const submission = useRef<AbortController | null>(null)

  useLayoutEffect(() => {
    if (confirming) cancelButton.current?.focus()
  }, [confirming])

  useEffect(() => () => submission.current?.abort(), [])

  useEffect(() => {
    const controller = new AbortController()

    async function loadPreview() {
      try {
        // 1. 只读查询选中条目的永久清理预检。
        const scope = await readPreview(target.id, controller.signal)
        // 2. 用版本核对列表中的名称仍对应本次预检，凭证只保留在内存。
        if (controller.signal.aborted) return
        if (scope.revision !== target.revision) {
          setPreview({ status: 'error', message: changedMessage, refreshList: true })
          return
        }
        setPreview({ status: 'ready', scope })
      } catch (error) {
        // 3. 显示失败原因，忽略窗口关闭后的结果。
        if (!controller.signal.aborted) {
          setPreview({ status: 'error', message: `无法读取清理预检。${apiErrorMessage(error)}` })
        }
      }
    }

    void loadPreview()
    return () => controller.abort()
  }, [attempt, readPreview, target.id, target.revision])

  function retryPreview() {
    setConfirming(false)
    setPreview({ status: 'loading' })
    setAttempt(previous => previous + 1)
  }

  async function submitConfirmation() {
    // 1. 仅确认按钮可进入这里，挡住连点和已过期的预检。
    if (!confirming || preview.status !== 'ready' || submission.current) return
    const reviewed = preview.scope
    if (Date.now() >= reviewed.expiresAt) {
      setPreview({ status: 'error', message: expiredMessage })
      return
    }
    const controller = new AbortController()
    submission.current = controller
    setSubmitting(true)
    let sent = false

    try {
      // 2. 只读复核当前资格、版本和展示范围，拦住已恢复或变化的目标。
      const current = await readPreview(reviewed.entryId, controller.signal)
      if (controller.signal.aborted) return
      if (Date.now() >= reviewed.expiresAt) {
        setPreview({ status: 'error', message: expiredMessage })
        return
      }
      if (current.revision !== reviewed.revision || current.fileCount !== reviewed.fileCount ||
          current.directoryCount !== reviewed.directoryCount || current.logicalBytes !== reviewed.logicalBytes) {
        setPreview({ status: 'error', message: changedMessage, refreshList: current.revision !== reviewed.revision })
        return
      }

      // 3. 使用原凭证发送一次。服务端在事务内按原凭证复核完整子树。
      const idempotencyKey = crypto.randomUUID()
      sent = true
      const receipt = await confirmPurge(reviewed, idempotencyKey)
      if (!controller.signal.aborted) onAccepted(receipt)
    } catch (error) {
      if (controller.signal.aborted) return
      if (error instanceof ApiError && (error.status === 404 || error.code === 'REVISION_CONFLICT' ||
          error.code === 'PURGE_PREVIEW_CHANGED')) {
        setPreview({ status: 'error', message: changedMessage,
          refreshList: error.status === 404 || error.code === 'REVISION_CONFLICT' })
      } else {
        const uncertain = sent && (!(error instanceof ApiError) || error.status >= 500 || error.status < 400)
        const message = uncertain ? '无法确认清理是否已受理，请刷新回收站并重新预检。' : apiErrorMessage(error)
        setPreview({ status: 'error', message, refreshList: uncertain })
      }
    } finally {
      submission.current = null
      if (!controller.signal.aborted) setSubmitting(false)
    }
  }

  function close() {
    if (!submission.current) onClose()
  }

  return (
    <Modal title={confirming ? '确认永久清理' : '永久清理预检'} open onCancel={close}
      keyboard={!submitting} closable={!submitting} mask={{ closable: false }} width={440}
      footer={(
        <>
          <Button ref={cancelButton} htmlType="button" onClick={close} disabled={submitting}>
            {confirming ? '取消' : '关闭预检'}
          </Button>
          {preview.status === 'ready' && (confirming ? (
            <Button danger type="primary" htmlType="button" disabled={submitting} loading={submitting}
              onKeyDown={event => { if (event.repeat) event.preventDefault() }}
              onClick={() => void submitConfirmation()}>确认清理</Button>
          ) : (
            <Button htmlType="button" onClick={() => setConfirming(true)}>继续确认</Button>
          ))}
        </>
      )}>
      <p><strong>{confirming ? '清理一经受理，无法通过回收站恢复。' : '只读预检，未执行清理。'}</strong></p>
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
          <p className="auth-error" role="alert">{preview.message}</p>
          {preview.refreshList ? <Button onClick={onRefreshTrash}>刷新回收站</Button> :
            <Button onClick={retryPreview}>{confirming ? '重新预检' : '重试预检'}</Button>}
        </>
      )}
    </Modal>
  )
}
