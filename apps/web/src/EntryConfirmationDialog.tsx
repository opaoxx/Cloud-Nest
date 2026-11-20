import { useRef, useState } from 'react'
import { Modal } from 'antd'
import { ApiError, apiErrorMessage } from './api/http'

export default function EntryConfirmationDialog({ title, message, confirmText, danger, onConfirm, onClose }: {
  title: string
  message: string
  confirmText: string
  danger?: boolean
  onConfirm: (idempotencyKey: string) => Promise<void>
  onClose: () => void
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

      // 2. 提交一次用户确认的操作，由调用方处理成功后续。
      await onConfirm(idempotencyKey)
    } catch (error) {
      // 3. 失败时保留确认框，并显示后端原因。
      const message = apiErrorMessage(error)
      setErrorMessage(error instanceof ApiError && error.status === 401 ? `${message}。请重新加载页面后再提交。` : message)
    } finally {
      inFlight.current = false
      setSubmitting(false)
    }
  }

  return (
    <Modal title={title} open onOk={() => void submit()}
      onCancel={() => { if (!inFlight.current) onClose() }}
      confirmLoading={submitting} closable={!submitting} keyboard={!submitting}
      okButtonProps={{ danger, disabled: submitting }} cancelButtonProps={{ disabled: submitting }}
      okText={confirmText} cancelText="取消" width={420}>
      <p style={{ overflowWrap: 'anywhere' }}>{message}</p>
      {errorMessage && <p className="auth-error" role="alert">{errorMessage}</p>}
    </Modal>
  )
}
