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

export interface TrashEntrySummary {
  id: string
  name: string
  revision: string
  originalPath: DirectoryBreadcrumb[]
  deletedAt: string
}

interface TrashPage {
  items: TrashEntrySummary[]
  nextCursor: string | null
}

export type TrashPageReader = (cursor: string | null, signal: AbortSignal) => Promise<TrashPage>

export interface PurgePreview {
  entryId: string
  revision: string
  digest: string
  expiresAt: number
  fileCount: number
  directoryCount: number
  logicalBytes: string
}

export type PurgePreviewReader = (entryId: string, signal: AbortSignal) => Promise<PurgePreview>
export type PurgeConfirmer = (preview: PurgePreview, idempotencyKey: string) => Promise<void>

export async function readPurgePreview(accessToken: string, entryId: string, signal: AbortSignal): Promise<PurgePreview> {
  // 1. 使用现有 GET 同时读取范围和确认凭证。
  const data = await requestData(`/api/v1/trash/${encodeURIComponent(entryId)}/purge-preview`, {
    method: 'GET',
    headers: { Authorization: `Bearer ${accessToken}` },
    signal,
  })

  // 2. 检查展示字段及凭证，保留十进制版本和逻辑字节的精度。
  const { file_count: fileCount, directory_count: directoryCount, logical_bytes: logicalBytes } = data
  const { revision, preview_digest: digest, expires_at: expiresAtText } = data
  const expiresAt = typeof expiresAtText === 'string' ? Date.parse(expiresAtText) : NaN
  if (data.entry_id !== entryId || !isByteCount(fileCount) || !isByteCount(directoryCount) ||
      fileCount + directoryCount < 1 || fileCount + directoryCount > 10000 ||
      typeof logicalBytes !== 'string' || !/^(0|[1-9][0-9]{0,19})$/.test(logicalBytes)) {
    throw new ApiError(200, '服务返回的清理预检范围有误。')
  }
  if (typeof revision !== 'string' || !/^[1-9][0-9]{0,19}$/.test(revision) ||
      typeof digest !== 'string' || digest.length > 1024 || !/^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{43}$/.test(digest) ||
      !Number.isFinite(expiresAt)) {
    throw new ApiError(200, '服务返回的清理预检凭证有误。')
  }
  return { entryId, revision, digest, expiresAt, fileCount, directoryCount, logicalBytes }
}

export async function confirmPurge(accessToken: string, preview: PurgePreview, idempotencyKey: string): Promise<void> {
  // 1. 原样提交用户看过的预检版本和凭证。
  const path = `/api/v1/trash/${encodeURIComponent(preview.entryId)}`
  const data = await requestData(path, {
    method: 'DELETE',
    headers: {
      Authorization: `Bearer ${accessToken}`,
      'Idempotency-Key': idempotencyKey,
      'X-CloudNest-Expected-Revision': preview.revision,
      'X-CloudNest-Purge-Preview': preview.digest,
    },
    redirect: 'error',
  }, 202)

  // 2. 核对受理的是同一目标，进度地址留给后续只读查询。
  if (data.entry_id !== preview.entryId || data.status !== 'PURGING' || data.progress_url !== `${path}/purge`) {
    throw new ApiError(202, '服务返回的清理受理结果有误。')
  }
}

interface EntryRestore {
  entryId: string
  expectedRevision: string
  idempotencyKey: string
}

export interface RestoredEntryLocation {
  name: string
  parentId: string
}

export type EntryRestorer = (restore: EntryRestore) => Promise<RestoredEntryLocation>

export async function restoreEntry(accessToken: string, restore: EntryRestore): Promise<RestoredEntryLocation> {
  // 1. 使用回收项 ID、原版本及幂等键请求恢复。
  const data = await requestData(`/api/v1/trash/${encodeURIComponent(restore.entryId)}/restore`, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${accessToken}`,
      'Content-Type': 'application/json',
      'Idempotency-Key': restore.idempotencyKey,
    },
    body: JSON.stringify({ expected_revision: restore.expectedRevision }),
    redirect: 'error',
  })

  // 2. 交付服务端返回的实际名称与目标父目录。
  const entry = data.entry
  if (!isRecord(entry) || entry.id !== restore.entryId || entry.status !== 'ACTIVE' ||
      typeof entry.name !== 'string' || !entry.name || typeof entry.parent_id !== 'string' || !entry.parent_id) {
    throw new ApiError(200, '服务返回的恢复结果格式有误。')
  }
  return { name: entry.name, parentId: entry.parent_id }
}

export async function listTrashPage(accessToken: string, cursor: string | null, signal: AbortSignal): Promise<TrashPage> {
  // 1. 使用当前访问令牌和可选游标读取回收站。
  const query = cursor === null ? '' : `?${new URLSearchParams({ cursor })}`
  const data = await requestData(`/api/v1/trash${query}`, {
    headers: { Authorization: `Bearer ${accessToken}` },
    signal,
  })

  // 2. 核对条目、原位置及回收时间。
  const nextCursor = data.next_cursor
  if (!Array.isArray(data.items) ||
      (nextCursor !== null && (typeof nextCursor !== 'string' || !nextCursor))) {
    throw new ApiError(200, '服务返回的回收站列表格式有误。')
  }
  const items = data.items.map((item: unknown): TrashEntrySummary => {
    if (!isRecord(item) || typeof item.id !== 'string' || !item.id ||
        typeof item.name !== 'string' || item.status !== 'TRASHED' ||
        typeof item.revision !== 'string' || !/^[1-9][0-9]{0,19}$/.test(item.revision) ||
        typeof item.deleted_at !== 'string' || !Number.isFinite(Date.parse(item.deleted_at))) {
      throw new ApiError(200, '服务返回的回收站条目格式有误。')
    }
    const originalPath = parseDirectoryBreadcrumbs(item.original_path)
    if (originalPath.at(-1)?.id !== item.parent_id) {
      throw new ApiError(200, '服务返回的回收站原位置有误。')
    }
    return { id: item.id, name: item.name, revision: item.revision, originalPath, deletedAt: item.deleted_at }
  })
  return { items, nextCursor }
}

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

interface EntryTrash {
  entryId: string
  expectedRevision: string
  idempotencyKey: string
}

export type EntryTrasher = (trash: EntryTrash) => Promise<void>

export async function trashEntry(accessToken: string, trash: EntryTrash): Promise<void> {
  // 1. 提交条目 ID、原版本和幂等键。
  const data = await requestData(`/api/v1/entries/${encodeURIComponent(trash.entryId)}/trash`, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${accessToken}`,
      'Content-Type': 'application/json',
      'Idempotency-Key': trash.idempotencyKey,
    },
    body: JSON.stringify({ expected_revision: trash.expectedRevision }),
    redirect: 'error',
  })

  // 2. 核对回收站快照。
  if (!isRecord(data.entry) || data.entry.id !== trash.entryId || data.entry.status !== 'TRASHED' ||
      typeof data.entry.revision !== 'string' || typeof data.entry.deleted_at !== 'string') {
    throw new ApiError(200, '服务返回的回收结果格式有误。')
  }
}

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
  const breadcrumbs = parseDirectoryBreadcrumbs(data.breadcrumbs)
  if (breadcrumbs.at(-1)?.id !== parentId) {
    throw new ApiError(200, '服务返回的目录路径与请求不符。')
  }
  return { items, breadcrumbs, nextCursor }
}

function parseDirectoryBreadcrumbs(value: unknown): DirectoryBreadcrumb[] {
  if (!Array.isArray(value) || value.length === 0) {
    throw new ApiError(200, '服务返回的目录路径格式有误。')
  }
  return value.map((crumb: unknown, index): DirectoryBreadcrumb => {
    if (!isRecord(crumb) || typeof crumb.id !== 'string' || !crumb.id ||
        typeof crumb.name !== 'string' || (index === 0 ? crumb.name !== '' : crumb.name === '')) {
      throw new ApiError(200, '服务返回的目录路径格式有误。')
    }
    return { id: crumb.id, name: crumb.name }
  })
}
