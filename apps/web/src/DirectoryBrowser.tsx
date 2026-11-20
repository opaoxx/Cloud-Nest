import { useEffect, useState } from 'react'
import { Button } from 'antd'
import type { DirectoryBreadcrumb, DirectoryEntry, DirectoryPageReader } from './api/drive'
import { apiErrorMessage } from './api/http'
import { formatSize } from './demo'

type DirectoryListing = {
  items: DirectoryEntry[]
  breadcrumbs: DirectoryBreadcrumb[]
  nextCursor: string | null
  status: 'loading' | 'ready' | 'error'
  errorMessage: string
}

type DirectoryRequest = {
  directoryId: string
  cursor: string | null
}

export default function DirectoryBrowser({ rootEntryId, readDirectoryPage }: {
  rootEntryId: string
  readDirectoryPage: DirectoryPageReader
}) {
  const [request, setRequest] = useState<DirectoryRequest>({ directoryId: rootEntryId, cursor: null })
  const [listing, setListing] = useState<DirectoryListing>({
    items: [], breadcrumbs: [], nextCursor: null, status: 'loading', errorMessage: '',
  })

  useEffect(() => {
    const controller = new AbortController()

    async function loadPage() {
      try {
        // 1. 读取目标目录的一页子项和服务端确认的路径。
        const page = await readDirectoryPage(request.directoryId, request.cursor, controller.signal)
        if (controller.signal.aborted) return

        // 2. 首页从空列表开始，后续页按字符串 ID 合并已有记录。
        setListing(previous => {
          const itemsById = new Map<string, DirectoryEntry>()
          if (request.cursor !== null) {
            for (const item of previous.items) itemsById.set(item.id, item)
          }
          for (const item of page.items) itemsById.set(item.id, item)
          return {
            items: [...itemsById.values()],
            breadcrumbs: page.breadcrumbs,
            nextCursor: page.nextCursor,
            status: 'ready',
            errorMessage: '',
          }
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
  }, [readDirectoryPage, request])

  function requestPage(cursor: string | null) {
    setListing(previous => ({ ...previous, status: 'loading', errorMessage: '' }))
    setRequest({ directoryId: request.directoryId, cursor })
  }

  // 1. 切换目录时从首页加载，清除上一目录的条目和分页游标。
  function openDirectory(directoryId: string) {
    if (directoryId === request.directoryId) return
    setListing(previous => ({
      ...previous, items: [], nextCursor: null, status: 'loading', errorMessage: '',
    }))
    setRequest({ directoryId, cursor: null })
  }

  // 2. 使用服务端确认的目录路径生成当前位置和上级入口。
  const isRoot = request.directoryId === rootEntryId
  const directoryLabel = isRoot ? '根目录' : '文件夹'

  return (
    <section className="directory-browser" aria-labelledby="directory-browser-title">
      <h2 id="directory-browser-title">目录</h2>
      {listing.breadcrumbs.length > 0 && (
        <nav aria-label="目录路径">
          <ol className="directory-path">
            {listing.breadcrumbs.map((crumb, index) => {
              const label = crumb.name || '根目录'
              const isCurrent = index === listing.breadcrumbs.length - 1 && crumb.id === request.directoryId
              return (
                <li key={crumb.id}>
                  {index > 0 && <span aria-hidden="true">/</span>}
                  {isCurrent ? <span aria-current="location">{label}</span> : (
                    <button type="button" className="directory-link" onClick={() => openDirectory(crumb.id)}>
                      {label}
                    </button>
                  )}
                </li>
              )
            })}
          </ol>
        </nav>
      )}
      {listing.items.length > 0 && (
        <table className="directory-items" aria-label={`${directoryLabel}条目`}>
          <thead><tr><th scope="col">名称</th><th scope="col">类型</th><th scope="col">大小</th></tr></thead>
          <tbody>
            {listing.items.map(item => (
              <tr key={item.id}>
                <td>{item.type === 'DIR' ? (
                  <button type="button" className="directory-link" onClick={() => openDirectory(item.id)}>
                    {item.name}
                  </button>
                ) : item.name}</td>
                <td>{item.type === 'DIR' ? '文件夹' : '文件'}</td>
                <td>{item.type === 'DIR' ? '—' : formatSize(item.sizeBytes)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {listing.status === 'loading' && <p role="status">正在读取{directoryLabel}…</p>}
      {listing.status === 'error' && (
        <>
          <p className="auth-error" role="alert">无法读取{directoryLabel}。{listing.errorMessage}</p>
          <Button onClick={() => requestPage(request.cursor)}>重试读取{directoryLabel}</Button>
        </>
      )}
      {listing.status === 'ready' && listing.items.length === 0 && (
        <p role="status">{isRoot ? '根目录为空。' : '此文件夹为空。'}</p>
      )}
      {listing.status === 'ready' && listing.nextCursor !== null && (
        <Button onClick={() => requestPage(listing.nextCursor)}>加载更多</Button>
      )}
    </section>
  )
}
