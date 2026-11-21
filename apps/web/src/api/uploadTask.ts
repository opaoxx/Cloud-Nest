import { ApiError, isRecord, requestData } from './http'
import { isUploadId } from './upload'
import type { SingleUploadDeclaration } from './upload'

export interface UploadTaskReceipt {
  taskId: string
  contentPath: string
}

export type SingleUploadTaskCreator = (declaration: SingleUploadDeclaration, idempotencyKey: string) => Promise<UploadTaskReceipt>

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
