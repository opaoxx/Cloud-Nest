import { ApiError, isByteCount, isRecord, requestData } from './http'

export interface DirectoryEntry {
  id: string
  name: string
  type: 'DIR' | 'FILE'
  sizeBytes: number
}

interface DirectoryPage {
  items: DirectoryEntry[]
  nextCursor: string | null
}

export async function listDirectoryPage(
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
  return { items, nextCursor }
}
