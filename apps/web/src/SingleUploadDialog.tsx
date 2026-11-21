import { useRef, useState } from 'react'
import { Button, Modal } from 'antd'
import { apiErrorMessage } from './api/http'
import { maxSingleUploadSizeMiB, prepareSingleUpload } from './api/upload'
import type { SingleUploadTaskCreator, UploadTaskReceipt } from './api/uploadTask'

export default function SingleUploadDialog({ parentId, targetPath, createUploadTask, onClose }: {
  parentId: string
  targetPath: string
  createUploadTask: SingleUploadTaskCreator
  onClose: () => void
}) {
  const [file, setFile] = useState<File | null>(null)
  const [receipt, setReceipt] = useState<UploadTaskReceipt | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [errorMessage, setErrorMessage] = useState('')
  const inFlight = useRef(false)
  const idempotencyKey = useRef<string | null>(null)
  const preparation = file ? prepareSingleUpload(parentId, file) : null
  const declaration = preparation && 'declaration' in preparation ? preparation.declaration : null

  function selectFile(selectedFile: File | null) {
    if (inFlight.current || receipt) return
    idempotencyKey.current = null
    setErrorMessage('')
    setFile(selectedFile)
  }

  async function submit() {
    // 1. 锁定本次选择，拦住连点和已受理任务。相同选择的手动重试沿用原键。
    if (!declaration || inFlight.current || receipt) return
    inFlight.current = true
    setSubmitting(true)
    setErrorMessage('')
    try {
      idempotencyKey.current ??= crypto.randomUUID()

      // 2. 发送一次创建请求，保存服务端返回的任务回执。
      const created = await createUploadTask(declaration, idempotencyKey.current)
      setReceipt(created)
    } catch (error) {
      // 3. 保留选择和幂等键，展示失败原因并等待用户手动重试。
      setErrorMessage(apiErrorMessage(error))
    } finally {
      inFlight.current = false
      setSubmitting(false)
    }
  }

  function close() {
    if (!inFlight.current) onClose()
  }

  return (
    <Modal title="准备单文件上传" open onCancel={close} width={440}
      closable={!submitting} keyboard={!submitting} mask={{ closable: !submitting }}
      footer={(
        <>
          <Button onClick={close} disabled={submitting}>关闭</Button>
          {!receipt && (
            <Button type="primary" onClick={() => void submit()} disabled={!declaration || submitting} loading={submitting}>
              创建上传任务
            </Button>
          )}
        </>
      )}>
      <div className="entry-name-form">
        <label htmlFor="single-upload-file">选择本地文件</label>
        <input id="single-upload-file" type="file" aria-describedby="single-upload-limit" disabled={submitting || !!receipt}
          onChange={event => selectFile(event.currentTarget.files?.item(0) ?? null)} />
        <small id="single-upload-limit">单文件上限 {maxSingleUploadSizeMiB} MiB。</small>
      </div>
      <dl className="auth-profile">
        <div><dt>目标目录</dt><dd>{targetPath}</dd></div>
        {declaration && (
          <>
            <div><dt>文件名</dt><dd>{declaration.name}</dd></div>
            <div><dt>文件大小</dt><dd>{declaration.size_bytes} 字节</dd></div>
          </>
        )}
        {receipt && <div><dt>任务 ID</dt><dd>{receipt.taskId}</dd></div>}
      </dl>
      {receipt ? <p role="status">任务已创建，等待上传文件内容。</p> :
        declaration && !submitting && <p role="status">文件已就绪，尚未上传。</p>}
      {preparation && 'error' in preparation && <p className="auth-error" role="alert">{preparation.error}</p>}
      {errorMessage && <p className="auth-error" role="alert">{errorMessage}</p>}
    </Modal>
  )
}
