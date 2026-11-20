import { ApiError, isByteCount, isRecord, requestData } from './http'

export interface DirectoryEntry {
  id: string
  name: string
  type: 'DIR' | 'FILE'
  sizeBytes: number
}

export interface DirectoryBreadcrumb {
  id: string
  name: string
}

interface DirectoryPage {
  items: DirectoryEntry[]
  breadcrumbs: DirectoryBreadcrumb[]
  nextCursor: string | null
}

export type DirectoryPageReader = (parentId: string, cursor: string | null, signal: AbortSignal) => Promise<DirectoryPage>

export interface DirectoryCreation {
  parentId: string
  name: string
  idempotencyKey: string
}

export type DirectoryCreator = (creation: DirectoryCreation) => Promise<void>

export async function createDirectory(accessToken: string, creation: DirectoryCreation): Promise<void> {
  // 1. 按正式契约提交父目录、原始名称和本次幂等键。
  const data = await requestData('/api/v1/directories', {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${accessToken}`,
      'Content-Type': 'application/json',
      'Idempotency-Key': creation.idempotencyKey,
    },
    body: JSON.stringify({ parent_id: creation.parentId, name: creation.name }),
    redirect: 'error',
  })

  // 2. 核对创建结果属于请求的父目录。
  const entry = data.entry
  if (!isRecord(entry) || typeof entry.id !== 'string' || !entry.id ||
      entry.parent_id !== creation.parentId || entry.entry_type !== 'DIR' || entry.status !== 'ACTIVE') {
    throw new ApiError(201, '服务返回的创建结果格式有误。')
  }
}

export async function readDirectoryPageWithRefresh(
  accessToken: string,
  parentId: string,
  cursor: string | null,
  signal: AbortSignal,
  refreshAccessToken: () => Promise<string>,
): Promise<DirectoryPage> {
  // 1. 用当前访问令牌读取原目录页。
  try {
    return await listDirectoryPage(accessToken, parentId, cursor, signal)
  } catch (error) {
    if (!(error instanceof ApiError && error.status === 401 && error.code === 'TOKEN_EXPIRED')) throw error
  }

  // 2. 只在目录接口明确报告令牌过期时刷新。
  signal.throwIfAborted()
  const renewedToken = await refreshAccessToken()

  // 3. 沿用原目录 ID 和游标重试一次。
  signal.throwIfAborted()
  return listDirectoryPage(renewedToken, parentId, cursor, signal)
}

async function listDirectoryPage(
  accessToken: string,
  parentId: string,
  cursor: string | null,
  signal: AbortSignal,
): Promise<DirectoryPage> {
  // 1. 使用当前会话和真实目录 ID 发出分页查询。
  const query = new URLSearchParams({ scope: 'directory', parent_id: parentId })
  if (cursor !== null) query.set('cursor', cursor)
  const data = await requestData(`/api/v1/entries?${query}`, {
    headers: { Authorization: `Bearer ${accessToken}` },
    signal,
  })

  // 2. 核对列表字段，保留字符串 ID 和后续游标。
  const nextCursor = data.next_cursor
  if (!Array.isArray(data.items) ||
      (nextCursor !== null && (typeof nextCursor !== 'string' || !nextCursor))) {
    throw new ApiError(200, '服务返回的目录列表格式有误。')
  }
  const items = data.items.map((item: unknown): DirectoryEntry => {
    if (!isRecord(item) || typeof item.id !== 'string' || item.parent_id !== parentId ||
        typeof item.name !== 'string' || (item.entry_type !== 'DIR' && item.entry_type !== 'FILE') ||
        item.status !== 'ACTIVE' || !isByteCount(item.size_bytes)) {
      throw new ApiError(200, '服务返回的目录条目格式有误。')
    }
    return { id: item.id, name: item.name, type: item.entry_type, sizeBytes: item.size_bytes }
  })
  // 3. 解析后端返回的根到当前目录路径，保留每一级的 ID 和名称。
  if (!Array.isArray(data.breadcrumbs) || data.breadcrumbs.length === 0) {
    throw new ApiError(200, '服务返回的目录路径格式有误。')
  }
  const breadcrumbs = data.breadcrumbs.map((crumb: unknown, index): DirectoryBreadcrumb => {
    if (!isRecord(crumb) || typeof crumb.id !== 'string' || !crumb.id ||
        typeof crumb.name !== 'string' || (index === 0 ? crumb.name !== '' : crumb.name === '')) {
      throw new ApiError(200, '服务返回的目录路径格式有误。')
    }
    return { id: crumb.id, name: crumb.name }
  })
  if (breadcrumbs.at(-1)?.id !== parentId) {
    throw new ApiError(200, '服务返回的目录路径与请求不符。')
  }
  return { items, breadcrumbs, nextCursor }
}
