import { useEffect, useRef, useState } from 'react'
import { Button, Modal } from 'antd'
import { ApiError, apiErrorMessage } from './api/http'
import { maxSingleUploadSizeMiB, prepareSingleUpload } from './api/upload'
import type {
  PublishedUploadEntry, PublishedUploadEntryReader, SingleUploadContentSender, SingleUploadTaskCanceller,
  SingleUploadTaskCreator, SingleUploadTaskReader, UploadTaskReceipt, UploadTaskSnapshot,
} from './api/uploadTask'

const uploadTaskStatusLabels: Record<UploadTaskSnapshot['status'], string> = {
  CREATING: '创建中',
  UPLOADING: '待接收或接收中',
  COMPLETING: '完成处理中',
  VERIFYING: '校验中',
  COMPLETED: '已完成',
  CANCELLING: '取消中',
  CANCELLED: '已取消',
  EXPIRING: '到期处理中',
  EXPIRED: '已过期',
  FAILED: '失败',
}

const cancellableStatuses: UploadTaskSnapshot['status'][] = ['CREATING', 'UPLOADING', 'COMPLETING', 'VERIFYING']

export default function SingleUploadDialog({
  parentId, targetPath, createUploadTask, sendUploadContent, readUploadTask, cancelUploadTask, readPublishedEntry,
  refreshProfile, onPublishedEntryRead, onClose,
}: {
  parentId: string
  targetPath: string
  createUploadTask: SingleUploadTaskCreator
  sendUploadContent: SingleUploadContentSender
  readUploadTask: SingleUploadTaskReader
  cancelUploadTask: SingleUploadTaskCanceller
  readPublishedEntry: PublishedUploadEntryReader
  refreshProfile: (signal: AbortSignal) => Promise<void>
  onPublishedEntryRead: () => void
  onClose: () => void
}) {
  const [file, setFile] = useState<File | null>(null)
  const [receipt, setReceipt] = useState<UploadTaskReceipt | null>(null)
  const [contentStatus, setContentStatus] = useState<'pending' | 'verifying' | 'needs-status-check'>('pending')
  const [submitting, setSubmitting] = useState(false)
  const [errorMessage, setErrorMessage] = useState('')
  const [taskSnapshot, setTaskSnapshot] = useState<UploadTaskSnapshot | null>(null)
  const [querying, setQuerying] = useState(false)
  const [queryError, setQueryError] = useState('')
  const [publishedEntry, setPublishedEntry] = useState<PublishedUploadEntry | null>(null)
  const [publishedError, setPublishedError] = useState('')
  const [quotaRefreshError, setQuotaRefreshError] = useState('')
  const [confirmingCancel, setConfirmingCancel] = useState(false)
  const [cancelNeedsStatusCheck, setCancelNeedsStatusCheck] = useState(false)
  const inFlight = useRef(false)
  const taskQuery = useRef<AbortController | null>(null)
  const idempotencyKey = useRef<string | null>(null)
  const preparation = file ? prepareSingleUpload(parentId, file) : null
  const declaration = preparation && 'declaration' in preparation ? preparation.declaration : null
  const canSendContent = contentStatus === 'pending' && (!taskSnapshot || taskSnapshot.status === 'UPLOADING')
  const canCancel = !taskSnapshot || cancellableStatuses.includes(taskSnapshot.status)

  useEffect(() => () => taskQuery.current?.abort(), [])

  function selectFile(selectedFile: File | null) {
    if (inFlight.current || receipt) return
    idempotencyKey.current = null
    setErrorMessage('')
    setFile(selectedFile)
  }

  async function loadPublishedEntry(entryId: string, signal: AbortSignal) {
    // 1. 读取结果文件，避免用任务里保存的发布名称代替当前详情。
    try {
      const entry = await readPublishedEntry(entryId, signal)
      if (signal.aborted) return

      // 2. 保存当前名称，再让原上传目录从首页读取真实列表。
      setPublishedEntry(entry)
      onPublishedEntryRead()
    } catch (error) {
      // 3. 详情读取失败只影响文件展示，保留已确认的任务状态。
      if (!signal.aborted) setPublishedError(apiErrorMessage(error))
    }
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

  async function submitContent() {
    // 1. 只发送当前任务对应的原文件，挡住并发点击。
    if (!file || !receipt || inFlight.current || taskQuery.current || !canSendContent || confirmingCancel || cancelNeedsStatusCheck) return
    inFlight.current = true
    setSubmitting(true)
    setErrorMessage('')
    setTaskSnapshot(null)
    setQueryError('')
    try {
      // 2. 收到同一任务的 202 后，展示校验中并结束正文提交。
      await sendUploadContent(receipt, file)
      setContentStatus('verifying')
    } catch (error) {
      // 3. 结果不明确或任务状态冲突时保留 ID，提示先查状态。
      const rejected = error instanceof ApiError && error.status >= 400 && error.status < 500 &&
        error.status !== 408 && error.status !== 409
      if (!rejected) setContentStatus('needs-status-check')
      const message = error instanceof ApiError ? error.message : '暂时无法确认文件接收结果。'
      setErrorMessage(message)
    } finally {
      inFlight.current = false
      setSubmitting(false)
    }
  }

  async function confirmCancellation() {
    // 1. 仅为当前任务确认取消，提交期间拦住连点和其他请求。
    if (!receipt || !confirmingCancel || !canCancel || cancelNeedsStatusCheck || inFlight.current || taskQuery.current) return
    inFlight.current = true
    setSubmitting(true)
    setErrorMessage('')
    setQueryError('')
    try {
      // 2. 使用取消接口返回的状态更新窗口。后台处理时继续保留查询入口。
      setTaskSnapshot(await cancelUploadTask(receipt.taskId))
    } catch (error) {
      // 3. 失败时先查同一任务，再由用户决定下一步，避免使用旧状态写入。
      setCancelNeedsStatusCheck(true)
      setErrorMessage(`无法确认上传取消。${apiErrorMessage(error)} 请先查询任务状态。`)
    } finally {
      inFlight.current = false
      setSubmitting(false)
      setConfirmingCancel(false)
    }
  }

  async function queryTask() {
    // 1. 锁定当前任务 ID，等在途请求结束后再允许查询。
    if (!receipt || inFlight.current || taskQuery.current || confirmingCancel) return
    const controller = new AbortController()
    taskQuery.current = controller
    setQuerying(true)
    setQueryError('')
    try {
      // 2. 使用最新的只读快照，替换接收回执上的旧状态。
      const snapshot = await readUploadTask(receipt.taskId, controller.signal)
      if (controller.signal.aborted) return
      setTaskSnapshot(snapshot)
      setCancelNeedsStatusCheck(false)
      setErrorMessage('')
      setPublishedEntry(null)
      setPublishedError('')
      if (snapshot.status === 'COMPLETED' && snapshot.resultEntryId) {
        await loadPublishedEntry(snapshot.resultEntryId, controller.signal)
      } else if (snapshot.status === 'CANCELLED') {
        await refreshCancelledUploadQuota(controller.signal)
      }
    } catch (error) {
      // 3. 读取失败时保留上一次结果，允许再次查询。
      if (!controller.signal.aborted) setQueryError(apiErrorMessage(error))
    } finally {
      taskQuery.current = null
      if (!controller.signal.aborted) setQuerying(false)
    }
  }

  async function refreshCancelledUploadQuota(signal: AbortSignal) {
    // 1. 按已取消的结果读取服务端当前配额，继续使用本次查询的取消信号。
    setQuotaRefreshError('')
    try {
      await refreshProfile(signal)
    } catch (error) {
      // 2. 配额读取失败时保留已取消状态，下次点查询即可重试读取。
      if (!signal.aborted) setQuotaRefreshError(apiErrorMessage(error))
    }
  }

  function close() {
    if (inFlight.current) return
    if (confirmingCancel) {
      setConfirmingCancel(false)
      return
    }
    taskQuery.current?.abort()
    onClose()
  }

  const publishedName = publishedEntry?.name ?? taskSnapshot?.resolvedName

  return (
    <Modal title="准备单文件上传" open onCancel={close} width={440}
      closable={!submitting} keyboard={!submitting} mask={{ closable: !submitting }}
      footer={confirmingCancel ? (
        <>
          <Button key="keep-upload" autoFocus onClick={close} disabled={submitting}>暂不取消</Button>
          <Button danger type="primary" onClick={() => void confirmCancellation()} disabled={submitting}>
            {submitting ? '正在提交取消…' : '确认取消上传'}
          </Button>
        </>
      ) : (
        <>
          <Button onClick={close} disabled={submitting}>关闭</Button>
          {!receipt && (
            <Button type="primary" onClick={() => void submit()} disabled={!declaration || submitting} loading={submitting}>
              创建上传任务
            </Button>
          )}
          {receipt && (
            <Button onClick={() => void queryTask()} disabled={submitting || querying}>
              {querying ? '查询中…' : '查询任务状态'}
            </Button>
          )}
          {receipt && canCancel && (
            <Button danger onClick={() => setConfirmingCancel(true)} disabled={submitting || querying || cancelNeedsStatusCheck}>
              取消上传任务
            </Button>
          )}
          {receipt && canSendContent && (
            <Button type="primary" onClick={() => void submitContent()}
              disabled={submitting || querying || cancelNeedsStatusCheck} loading={submitting}>
              上传文件内容
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
        {taskSnapshot && <div><dt>任务状态</dt><dd role="status">{uploadTaskStatusLabels[taskSnapshot.status]}</dd></div>}
        {taskSnapshot?.status === 'COMPLETED' && (
          <>
            {publishedName && <div><dt>{publishedEntry ? '当前文件名' : '发布时文件名'}</dt><dd>{publishedName}</dd></div>}
            <div><dt>文件 ID</dt><dd>{taskSnapshot.resultEntryId}</dd></div>
          </>
        )}
      </dl>
      {confirmingCancel && <p>确定取消此文件的上传任务？提交后由后台处理，可查询任务状态查看结果。</p>}
      {!receipt && declaration && !submitting && <p role="status">文件已就绪，尚未上传。</p>}
      {receipt && !taskSnapshot && contentStatus === 'pending' && !confirmingCancel && !cancelNeedsStatusCheck && (
        <p role="status">{submitting ? '正在发送文件内容…' : '任务已创建，等待上传文件内容。'}</p>
      )}
      {!taskSnapshot && contentStatus === 'verifying' && <p role="status">文件内容已接收，校验中。</p>}
      {!taskSnapshot && contentStatus === 'needs-status-check' && <p>请保留任务 ID，查询任务状态后再重试。</p>}
      {taskSnapshot?.status === 'CANCELLING' && <p>取消已受理，后台正在处理。可查询任务状态确认结果。</p>}
      {taskSnapshot?.lastError && <p className="auth-error" role="alert">{taskSnapshot.lastError}</p>}
      {queryError && <p className="auth-error" role="alert">无法查询上传任务。{queryError}</p>}
      {quotaRefreshError && <p className="auth-error" role="alert">无法刷新配额。{quotaRefreshError} 可再次查询任务状态。</p>}
      {publishedError && <p className="auth-error" role="alert">无法读取发布文件。{publishedError}</p>}
      {publishedEntry && publishedEntry.parentId !== parentId && <p>文件当前位于其他目录。</p>}
      {preparation && 'error' in preparation && <p className="auth-error" role="alert">{preparation.error}</p>}
      {errorMessage && <p className="auth-error" role="alert">{errorMessage}</p>}
    </Modal>
  )
}
