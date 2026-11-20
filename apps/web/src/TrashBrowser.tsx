import { useEffect, useState } from 'react'
import { Button } from 'antd'
import type { DirectoryBreadcrumb, DirectoryPageReader, EntryRestorer, RestoredEntryLocation, TrashEntrySummary, TrashPageReader } from './api/drive'
import { apiErrorMessage } from './api/http'
import { formatDate } from './demo'
import EntryConfirmationDialog from './EntryConfirmationDialog'

type TrashListing = {
  items: TrashEntrySummary[]
  nextCursor: string | null
  status: 'loading' | 'ready' | 'error'
  errorMessage: string
}

function formatDirectoryPath(path: DirectoryBreadcrumb[]) {
  return path.map(directory => directory.name || '根目录').join(' / ')
}

export default function TrashBrowser({ rootEntryId, readTrashPage, readDirectoryPage, restoreEntry }: {
  rootEntryId: string
  readTrashPage: TrashPageReader
  readDirectoryPage: DirectoryPageReader
  restoreEntry: EntryRestorer
}) {
  const [request, setRequest] = useState<{ cursor: string | null }>({ cursor: null })
  const [listing, setListing] = useState<TrashListing>({
    items: [], nextCursor: null, status: 'loading', errorMessage: '',
  })
  const [restoreTarget, setRestoreTarget] = useState<TrashEntrySummary | null>(null)
  const [restored, setRestored] = useState<RestoredEntryLocation | null>(null)
  const [restoredPath, setRestoredPath] = useState<DirectoryBreadcrumb[] | null>(null)

  useEffect(() => {
    const controller = new AbortController()

    async function loadPage() {
      try {
        // 1. 进入回收站时读取首页，续页使用服务端游标。
        const page = await readTrashPage(request.cursor, controller.signal)
        if (controller.signal.aborted) return

        // 2. 续页按条目 ID 合并，避免排序变化时重复展示。
        setListing(previous => {
          const entries = new Map<string, TrashEntrySummary>()
          if (request.cursor !== null) {
            for (const entry of previous.items) entries.set(entry.id, entry)
          }
          for (const entry of page.items) entries.set(entry.id, entry)
          return { items: [...entries.values()], nextCursor: page.nextCursor, status: 'ready', errorMessage: '' }
        })
      } catch (error) {
        // 3. 展示读取错误，由用户决定何时从首页重试。
        if (!controller.signal.aborted) {
          setListing(previous => ({ ...previous, status: 'error', errorMessage: apiErrorMessage(error) }))
        }
      }
    }

    void loadPage()
    return () => controller.abort()
  }, [readTrashPage, request])

  useEffect(() => {
    if (!restored || restored.parentId === rootEntryId) return
    const controller = new AbortController()
    // 从实际父目录读取路径。路径读取失败时保留已确认的目录 ID。
    void readDirectoryPage(restored.parentId, null, controller.signal).then(page => {
      if (!controller.signal.aborted) setRestoredPath(page.breadcrumbs)
    }).catch(() => {
      if (!controller.signal.aborted) setRestoredPath(null)
    })
    return () => controller.abort()
  }, [readDirectoryPage, restored, rootEntryId])

  function requestPage(cursor: string | null) {
    setListing(previous => ({ ...previous, status: 'loading', errorMessage: '' }))
    setRequest({ cursor })
  }

  async function confirmRestore(target: TrashEntrySummary, idempotencyKey: string) {
    // 1. 用打开确认框时的回收项 ID 和版本提交一次恢复。
    const result = await restoreEntry({ entryId: target.id, expectedRevision: target.revision, idempotencyKey })

    // 2. 展示服务端确认的位置，并重读回收站首页。
    setRestoreTarget(null)
    setRestoredPath(null)
    setRestored(result)
    requestPage(null)
  }

  const restoredPosition = restored?.parentId === rootEntryId ? '根目录' :
    restoredPath ? formatDirectoryPath(restoredPath) : `目录 ID ${restored?.parentId}`

  return (
    <section className="directory-browser" aria-labelledby="trash-browser-title">
      <h2 id="trash-browser-title">回收站</h2>
      {restored && <p role="status">已将「{restored.name}」恢复到「{restoredPosition}」。</p>}
      {restoreTarget && (
        <EntryConfirmationDialog title="确认恢复" message={`恢复「${restoreTarget.name}」？`}
          confirmText="恢复条目" onClose={() => setRestoreTarget(null)}
          onConfirm={idempotencyKey => confirmRestore(restoreTarget, idempotencyKey)} />
      )}
      {listing.items.length > 0 && (
        <table className="directory-items trash-items" aria-label="已回收条目">
          <thead><tr><th scope="col">名称</th><th scope="col">原位置</th><th scope="col">回收时间</th><th scope="col">操作</th></tr></thead>
          <tbody>
            {listing.items.map(entry => (
              <tr key={entry.id}>
                <td>{entry.name}</td>
                <td>{formatDirectoryPath(entry.originalPath)}</td>
                <td><time dateTime={entry.deletedAt}>{formatDate(Date.parse(entry.deletedAt))}</time></td>
                <td><button type="button" className="directory-link" aria-label={`恢复 ${entry.name}`}
                  disabled={listing.status !== 'ready'} onClick={() => setRestoreTarget(entry)}>恢复</button></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {listing.status === 'loading' && <p role="status">正在读取回收站…</p>}
      {listing.status === 'error' && (
        <>
          <p className="auth-error" role="alert">无法读取回收站。{listing.errorMessage}</p>
          <Button onClick={() => requestPage(null)}>重试读取回收站</Button>
        </>
      )}
      {listing.status === 'ready' && listing.items.length === 0 && (
        <p role="status">{listing.nextCursor === null ? '回收站为空。' : '本页暂无可显示条目。'}</p>
      )}
      {listing.status === 'ready' && listing.nextCursor !== null && (
        <Button onClick={() => requestPage(listing.nextCursor)}>加载更多回收项</Button>
      )}
    </section>
  )
}
