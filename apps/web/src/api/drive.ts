import { ApiError, isByteCount, isRecord, requestData } from './http'

export interface DirectoryEntry {
  id: string
  name: string
  type: 'DIR' | 'FILE'
  sizeBytes: number
  revision: string
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

interface EntryRename {
  entryId: string
  expectedRevision: string
  name: string
  idempotencyKey: string
}

export type EntryRenamer = (rename: EntryRename) => Promise<void>

interface EntryMove {
  entryId: string
  targetParentId: string
  expectedRevision: string
  idempotencyKey: string
}

export type EntryMover = (move: EntryMove) => Promise<void>

export async function moveEntry(accessToken: string, move: EntryMove): Promise<void> {
  // 1. 使用现有移动接口提交目标父目录及预期版本。
  const data = await requestData(`/api/v1/entries/${encodeURIComponent(move.entryId)}/move`, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${accessToken}`,
      'Content-Type': 'application/json',
      'Idempotency-Key': move.idempotencyKey,
    },
    body: JSON.stringify({ target_parent_id: move.targetParentId, expected_revision: move.expectedRevision }),
    redirect: 'error',
  })

  // 2. 核对响应的条目及目标父目录。
  if (!isRecord(data.entry) || data.entry.id !== move.entryId ||
      data.entry.parent_id !== move.targetParentId || data.entry.status !== 'ACTIVE' ||
      typeof data.entry.revision !== 'string') {
    throw new ApiError(200, '服务返回的移动结果格式有误。')
  }
}

export async function renameEntry(accessToken: string, rename: EntryRename): Promise<void> {
  // 1. 用条目 ID、原版本和幂等键提交一次重命名。
  const data = await requestData(`/api/v1/entries/${encodeURIComponent(rename.entryId)}`, {
    method: 'PATCH',
    headers: {
      Authorization: `Bearer ${accessToken}`,
      'Content-Type': 'application/json',
      'Idempotency-Key': rename.idempotencyKey,
    },
    body: JSON.stringify({ expected_revision: rename.expectedRevision, name: rename.name }),
    redirect: 'error',
  })

  // 2. 核对成功响应对应原条目。
  if (!isRecord(data.entry) || data.entry.id !== rename.entryId ||
      data.entry.status !== 'ACTIVE' || typeof data.entry.revision !== 'string') {
    throw new ApiError(200, '服务返回的重命名结果格式有误。')
  }
}

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
        item.status !== 'ACTIVE' || !isByteCount(item.size_bytes) ||
        typeof item.revision !== 'string' || !/^[1-9][0-9]{0,19}$/.test(item.revision)) {
      throw new ApiError(200, '服务返回的目录条目格式有误。')
    }
    return { id: item.id, name: item.name, type: item.entry_type, sizeBytes: item.size_bytes, revision: item.revision }
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
