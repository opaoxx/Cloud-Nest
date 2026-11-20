import { useEffect, useState } from 'react'
import { Button } from 'antd'
import type { TrashEntrySummary, TrashPageReader } from './api/drive'
import { apiErrorMessage } from './api/http'
import { formatDate } from './demo'

type TrashListing = {
  items: TrashEntrySummary[]
  nextCursor: string | null
  status: 'loading' | 'ready' | 'error'
  errorMessage: string
}

export default function TrashBrowser({ readTrashPage }: { readTrashPage: TrashPageReader }) {
  const [request, setRequest] = useState<{ cursor: string | null }>({ cursor: null })
  const [listing, setListing] = useState<TrashListing>({
    items: [], nextCursor: null, status: 'loading', errorMessage: '',
  })

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

  function requestPage(cursor: string | null) {
    setListing(previous => ({ ...previous, status: 'loading', errorMessage: '' }))
    setRequest({ cursor })
  }

  return (
    <section className="directory-browser" aria-labelledby="trash-browser-title">
      <h2 id="trash-browser-title">回收站</h2>
      {listing.items.length > 0 && (
        <table className="directory-items trash-items" aria-label="已回收条目">
          <thead><tr><th scope="col">名称</th><th scope="col">原位置</th><th scope="col">回收时间</th></tr></thead>
          <tbody>
            {listing.items.map(entry => (
              <tr key={entry.id}>
                <td>{entry.name}</td>
                <td>{entry.originalPath.map(directory => directory.name || '根目录').join(' / ')}</td>
                <td><time dateTime={entry.deletedAt}>{formatDate(Date.parse(entry.deletedAt))}</time></td>
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
