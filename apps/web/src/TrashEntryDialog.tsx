import { useRef, useState } from 'react'
import { Modal } from 'antd'
import type { DirectoryEntry, EntryTrasher } from './api/drive'
import { ApiError, apiErrorMessage } from './api/http'

export default function TrashEntryDialog({ entry, trashEntry, onClose, onSaved }: {
  entry: DirectoryEntry
  trashEntry: EntryTrasher
  onClose: () => void
  onSaved: () => void
}) {
  const [submitting, setSubmitting] = useState(false)
  const [errorMessage, setErrorMessage] = useState('')
  const inFlight = useRef(false)
  const attemptKey = useRef<string | null>(null)

  async function submit() {
    // 1. 提交期间挡住连点，相同确认框的手动重试复用原键。
    if (inFlight.current) return
    inFlight.current = true
    setSubmitting(true)
    setErrorMessage('')
    try {
      const idempotencyKey = attemptKey.current ?? crypto.randomUUID()
      attemptKey.current = idempotencyKey

      // 2. 只发送一次回收请求，使用打开确认框时的条目版本。
      await trashEntry({ entryId: entry.id, expectedRevision: entry.revision, idempotencyKey })

      // 3. 成功后重读目录，失败时保留确认框并显示原因。
      onSaved()
    } catch (error) {
      const message = apiErrorMessage(error)
      setErrorMessage(error instanceof ApiError && error.status === 401 ? `${message}。请重新加载页面后再提交。` : message)
    } finally {
      inFlight.current = false
      setSubmitting(false)
    }
  }

  return (
    <Modal title="确认移入回收站" open onOk={() => void submit()}
      onCancel={() => { if (!inFlight.current) onClose() }}
      confirmLoading={submitting} closable={!submitting} keyboard={!submitting}
      okButtonProps={{ danger: true, disabled: submitting }} cancelButtonProps={{ disabled: submitting }}
      okText="移入回收站" cancelText="取消" width={420}>
      <p style={{ overflowWrap: 'anywhere' }}>将「{entry.name}」移入回收站？</p>
      {errorMessage && <p className="auth-error" role="alert">{errorMessage}</p>}
    </Modal>
  )
}
