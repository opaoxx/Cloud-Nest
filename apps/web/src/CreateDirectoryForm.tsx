import { useRef, useState } from 'react'
import { Button, Input, Modal } from 'antd'
import type { DirectoryCreation, DirectoryCreator } from './api/drive'
import { ApiError, apiErrorMessage } from './api/http'

export default function CreateDirectoryForm({ parentId, parentName, disabled, createDirectory, onCreated }: {
  parentId: string
  parentName: string
  disabled: boolean
  createDirectory: DirectoryCreator
  onCreated: () => void
}) {
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [errorMessage, setErrorMessage] = useState('')
  const inFlight = useRef(false)
  const lastAttempt = useRef<DirectoryCreation | null>(null)

  function openForm() {
    lastAttempt.current = null
    setName('')
    setErrorMessage('')
    setOpen(true)
  }

  async function submit() {
    // 1. 检查名称与提交状态，为本次创建准备幂等键。
    if (disabled || inFlight.current) return
    if (!name.trim()) {
      setErrorMessage('请输入文件夹名称。')
      return
    }
    inFlight.current = true
    setSubmitting(true)
    setErrorMessage('')

    try {
      const previous = lastAttempt.current
      const creation = previous?.name === name ? previous : {
        parentId, name, idempotencyKey: crypto.randomUUID(),
      }
      lastAttempt.current = creation

      // 2. 发送一次创建请求，由后端校验会话、目录归属和名称。
      await createDirectory(creation)

      // 3. 成功后重读当前目录，失败时保留表单并显示原因。
      setOpen(false)
      onCreated()
    } catch (error) {
      const message = apiErrorMessage(error)
      setErrorMessage(error instanceof ApiError && error.status === 401 ? `${message}。请重新加载页面后再提交。` : message)
    } finally {
      inFlight.current = false
      setSubmitting(false)
    }
  }

  return (
    <>
      <Button className="create-directory-trigger" disabled={disabled} onClick={openForm}>新建文件夹</Button>
      <Modal title="新建文件夹" open={open} onOk={() => void submit()}
        onCancel={() => { if (!inFlight.current) setOpen(false) }}
        confirmLoading={submitting} closable={!submitting} keyboard={!submitting}
        cancelButtonProps={{ disabled: submitting }} okText="创建文件夹" cancelText="取消"
        width={420} destroyOnHidden>
        <div className="create-directory-form">
          <label htmlFor="new-directory-name">文件夹名称</label>
          <Input id="new-directory-name" autoFocus value={name} disabled={submitting}
            status={errorMessage ? 'error' : undefined} onPressEnter={() => void submit()}
            onChange={event => { setName(event.target.value); setErrorMessage('') }} />
          {errorMessage && <p className="auth-error" role="alert">{errorMessage}</p>}
          <small>将在「{parentName}」中创建。</small>
        </div>
      </Modal>
    </>
  )
}
