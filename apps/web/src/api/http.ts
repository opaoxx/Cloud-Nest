export class ApiError extends Error {
  constructor(readonly status: number, message: string, readonly code?: string) {
    super(message)
  }
}

export function apiErrorMessage(error: unknown): string {
  return error instanceof ApiError ? error.message : '暂时无法连接到服务，请重试。'
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

export function isByteCount(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
}

export async function requestData(path: string, init: RequestInit, expectedStatus?: number): Promise<Record<string, unknown>> {
  const { data } = await requestDataResponse(path, init, expectedStatus === undefined ? undefined : [expectedStatus])
  return data
}

export async function requestDataResponse(path: string, init: RequestInit, expectedStatuses?: readonly number[]) {
  const response = await fetch(path, { ...init, credentials: 'same-origin', cache: 'no-store' })
  const payload: unknown = await response.json().catch(() => null)
  if (!response.ok) {
    const error = isRecord(payload) && isRecord(payload.error) ? payload.error : null
    const message = error && typeof error.message === 'string' ? error.message : '服务暂时无法完成请求，请重试。'
    const code = error && typeof error.code === 'string' ? error.code : undefined
    throw new ApiError(response.status, message, code)
  }
  if (expectedStatuses && !expectedStatuses.includes(response.status)) {
    throw new ApiError(response.status, '服务返回的响应状态有误。')
  }
  if (!isRecord(payload) || !isRecord(payload.data)) {
    throw new ApiError(response.status, '服务返回的数据格式有误。')
  }
  return { status: response.status, data: payload.data }
}
