import { useEffect, useState } from 'react'
import { Button } from 'antd'
import { listDirectoryPage } from './api/drive'
import type { DirectoryEntry } from './api/drive'
import { apiErrorMessage } from './api/http'
import { formatSize } from './demo'

type DirectoryListing = {
  items: DirectoryEntry[]
  nextCursor: string | null
  status: 'loading' | 'ready' | 'error'
  errorMessage: string
}

export default function RootDirectory({ accessToken, rootEntryId }: { accessToken: string; rootEntryId: string }) {
  const [request, setRequest] = useState<{ cursor: string | null }>({ cursor: null })
  const [listing, setListing] = useState<DirectoryListing>({
    items: [], nextCursor: null, status: 'loading', errorMessage: '',
  })

  useEffect(() => {
    const controller = new AbortController()

    async function loadPage() {
      try {
        // 1. 从本人资料取根目录 ID，读取该目录的一页子项。
        const page = await listDirectoryPage(accessToken, rootEntryId, request.cursor, controller.signal)
        if (controller.signal.aborted) return

        // 2. 首页从空列表开始，后续页按字符串 ID 合并已有记录。
        setListing(previous => {
          const itemsById = new Map<string, DirectoryEntry>()
          if (request.cursor !== null) {
            for (const item of previous.items) itemsById.set(item.id, item)
          }
          for (const item of page.items) itemsById.set(item.id, item)
          return { items: [...itemsById.values()], nextCursor: page.nextCursor, status: 'ready', errorMessage: '' }
        })
      } catch (error) {
        // 3. 读取失败时保留已展示条目，允许重试当前页。
        if (!controller.signal.aborted) {
          setListing(previous => ({ ...previous, status: 'error', errorMessage: apiErrorMessage(error) }))
        }
      }
    }

    void loadPage()
    return () => controller.abort()
  }, [accessToken, rootEntryId, request])

  function requestPage(cursor: string | null) {
    setListing(previous => ({ ...previous, status: 'loading', errorMessage: '' }))
    setRequest({ cursor })
  }

  return (
    <section className="root-directory" aria-labelledby="root-directory-title">
      <h2 id="root-directory-title">根目录</h2>
      {listing.items.length > 0 && (
        <table className="root-directory-items" aria-label="根目录条目">
          <thead><tr><th scope="col">名称</th><th scope="col">类型</th><th scope="col">大小</th></tr></thead>
          <tbody>
            {listing.items.map(item => (
              <tr key={item.id}>
                <td>{item.name}</td>
                <td>{item.type === 'DIR' ? '文件夹' : '文件'}</td>
                <td>{item.type === 'DIR' ? '—' : formatSize(item.sizeBytes)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {listing.status === 'loading' && <p role="status">正在读取根目录…</p>}
      {listing.status === 'error' && (
        <>
          <p className="auth-error" role="alert">无法读取根目录。{listing.errorMessage}</p>
          <Button onClick={() => requestPage(request.cursor)}>重试读取根目录</Button>
        </>
      )}
      {listing.status === 'ready' && listing.items.length === 0 && <p role="status">根目录为空。</p>}
      {listing.status === 'ready' && listing.nextCursor !== null && (
        <Button onClick={() => requestPage(listing.nextCursor)}>加载更多</Button>
      )}
    </section>
  )
}
