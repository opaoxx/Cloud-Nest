import { useRef, useState } from 'react'
import { Input, Modal } from 'antd'
import { ApiError, apiErrorMessage } from './api/http'

export default function EntryNameDialog({
  title, inputLabel, confirmLabel, hint, initialName, submitName, onClose, onSaved,
}: {
  title: string
  inputLabel: string
  confirmLabel: string
  hint: string
  initialName: string
  submitName: (name: string, idempotencyKey: string) => Promise<void>
  onClose: () => void
  onSaved: () => void
}) {
  const [name, setName] = useState(initialName)
  const [submitting, setSubmitting] = useState(false)
  const [errorMessage, setErrorMessage] = useState('')
  const inFlight = useRef(false)
  const lastAttempt = useRef<{ name: string; idempotencyKey: string } | null>(null)

  async function submit() {
    // 1. 检查名称与提交状态，相同输入的手动重试沿用原键。
    if (inFlight.current) return
    if (!name.trim()) {
      setErrorMessage(`请输入${inputLabel}。`)
      return
    }
    inFlight.current = true
    setSubmitting(true)
    setErrorMessage('')

    try {
      const previous = lastAttempt.current
      const attempt = previous?.name === name ? previous : {
        name, idempotencyKey: crypto.randomUUID(),
      }
      lastAttempt.current = attempt

      // 2. 只提交一次名称操作，沿用后端的认证和冲突检查。
      await submitName(attempt.name, attempt.idempotencyKey)

      // 3. 成功后通知列表重读，失败时保留表单和用户输入。
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
    <Modal title={title} open onOk={() => void submit()}
      onCancel={() => { if (!inFlight.current) onClose() }}
      confirmLoading={submitting} closable={!submitting} keyboard={!submitting}
      cancelButtonProps={{ disabled: submitting }} okText={confirmLabel} cancelText="取消" width={420}>
      <div className="entry-name-form">
        <label htmlFor="entry-action-name">{inputLabel}</label>
        <Input id="entry-action-name" autoFocus value={name} disabled={submitting}
          status={errorMessage ? 'error' : undefined} onPressEnter={() => void submit()}
          onChange={event => { setName(event.target.value); setErrorMessage('') }} />
        {errorMessage && <p className="auth-error" role="alert">{errorMessage}</p>}
        <small>{hint}</small>
      </div>
    </Modal>
  )
}
