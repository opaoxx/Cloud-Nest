import { ApiError, isRecord, requestData } from './http'
import { isUploadId } from './upload'
import type { SingleUploadDeclaration } from './upload'

export interface UploadTaskReceipt {
  taskId: string
  contentPath: string
}

export type SingleUploadTaskCreator = (declaration: SingleUploadDeclaration, idempotencyKey: string) => Promise<UploadTaskReceipt>
export type SingleUploadContentSender = (receipt: UploadTaskReceipt, file: File) => Promise<void>

const uploadTaskStatuses = [
  'CREATING', 'UPLOADING', 'COMPLETING', 'VERIFYING', 'COMPLETED',
  'CANCELLING', 'CANCELLED', 'EXPIRING', 'EXPIRED', 'FAILED',
] as const
type UploadTaskStatus = typeof uploadTaskStatuses[number]

export interface UploadTaskSnapshot {
  status: UploadTaskStatus
  resultEntryId: string | null
  resolvedName: string | null
  lastError: string | null
}

export type SingleUploadTaskReader = (taskId: string, signal: AbortSignal) => Promise<UploadTaskSnapshot>

function isUploadTaskStatus(value: unknown): value is UploadTaskStatus {
  return uploadTaskStatuses.some(status => status === value)
}

export async function createSingleUploadTask(accessToken: string, declaration: SingleUploadDeclaration, idempotencyKey: string): Promise<UploadTaskReceipt> {
  // 1. 仅提交创建任务的文件声明和本次操作的幂等键。
  const data = await requestData('/api/v1/uploads', {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${accessToken}`,
      'Content-Type': 'application/json',
      'Idempotency-Key': idempotencyKey,
    },
    body: JSON.stringify(declaration),
    redirect: 'error',
  }, 201)

  // 2. 核对任务对应本次声明，内容接收地址必须指向同源的同一任务。
  const task = data.task
  if (!isRecord(task) || !isUploadId(task.id) || task.parent_id !== declaration.parent_id ||
      task.size_bytes !== declaration.size_bytes || task.status !== 'UPLOADING' ||
      task.upload_mode !== 'SINGLE' || data.upload_mode !== 'SINGLE' ||
      data.content_path !== `/api/v1/uploads/${task.id}/content`) {
    throw new ApiError(201, '服务返回的上传任务回执有误。')
  }
  return { taskId: task.id, contentPath: data.content_path }
}

export async function sendSingleUploadContent(accessToken: string, receipt: UploadTaskReceipt, file: File): Promise<void> {
  // 1. 将发送地址限定在同源的当前上传任务。
  if (!isUploadId(receipt.taskId) || receipt.contentPath !== `/api/v1/uploads/${receipt.taskId}/content`) {
    throw new ApiError(0, '上传任务的内容接收地址有误。')
  }

  // 2. 直接把 File 作为正文，浏览器根据文件大小生成 Content-Length。
  const data = await requestData(receipt.contentPath, {
    method: 'PUT',
    headers: {
      Authorization: `Bearer ${accessToken}`,
      'Content-Type': 'application/octet-stream',
    },
    body: file,
    redirect: 'error',
  }, 202)

  // 3. 只接受同一个任务的校验受理结果。
  if (data.task_id !== receipt.taskId || data.status !== 'VERIFYING') {
    throw new ApiError(202, '服务返回的上传接收回执有误。')
  }
}

export async function readSingleUploadTask(accessToken: string, taskId: string, signal: AbortSignal): Promise<UploadTaskSnapshot> {
  // 1. 使用当前任务 ID，用 GET 获取服务端快照。
  if (!isUploadId(taskId)) throw new ApiError(0, '上传任务 ID 有误。')
  const data = await requestData(`/api/v1/uploads/${taskId}`, {
    method: 'GET',
    headers: { Authorization: `Bearer ${accessToken}` },
    redirect: 'error',
    signal,
  }, 200)

  // 2. 核对任务及展示字段，已完成时必须有发布结果 ID。
  const { status, result_entry_id: resultEntryId, resolved_name: resolvedName, last_error: lastError } = data
  const lastErrorMessage = isRecord(lastError) && typeof lastError.message === 'string' ? lastError.message : null
  if (data.id !== taskId || data.upload_mode !== 'SINGLE' || !isUploadTaskStatus(status) ||
      (resultEntryId !== null && !isUploadId(resultEntryId)) || (status === 'COMPLETED' && resultEntryId === null) ||
      (resolvedName !== null && (typeof resolvedName !== 'string' || !resolvedName)) ||
      (lastError !== null && !lastErrorMessage)) {
    throw new ApiError(200, '服务返回的上传任务状态有误。')
  }
  return { status, resultEntryId, resolvedName, lastError: lastErrorMessage }
}
