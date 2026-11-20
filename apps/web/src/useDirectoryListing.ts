import { useEffect, useState } from 'react'
import type { DirectoryBreadcrumb, DirectoryEntry, DirectoryPageReader } from './api/drive'
import { apiErrorMessage } from './api/http'

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

export function useDirectoryListing(rootEntryId: string, readDirectoryPage: DirectoryPageReader) {
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

  function openDirectory(directoryId: string) {
    if (directoryId === request.directoryId) return
    // 目录改变后从首页开始，让旧目录的请求退出。
    setListing(previous => ({
      ...previous, items: [], nextCursor: null, status: 'loading', errorMessage: '',
    }))
    setRequest({ directoryId, cursor: null })
  }

  return { request, listing, requestPage, openDirectory }
}
