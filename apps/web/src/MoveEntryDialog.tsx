import { useRef, useState } from 'react'
import { Button, Modal } from 'antd'
import type { DirectoryEntry, DirectoryPageReader, EntryMover } from './api/drive'
import { ApiError, apiErrorMessage } from './api/http'
import { useDirectoryListing } from './useDirectoryListing'

export default function MoveEntryDialog({ entry, rootEntryId, readDirectoryPage, moveEntry, onClose, onSaved }: {
  entry: DirectoryEntry
  rootEntryId: string
  readDirectoryPage: DirectoryPageReader
  moveEntry: EntryMover
  onClose: () => void
  onSaved: () => void
}) {
  // 1. 从根目录浏览可见的目标文件夹，沿用现有目录分页。
  const { request, listing, requestPage, openDirectory } = useDirectoryListing(rootEntryId, readDirectoryPage)
  const [submitting, setSubmitting] = useState(false)
  const [moveError, setMoveError] = useState('')
  const inFlight = useRef(false)
  const lastAttempt = useRef<{ targetParentId: string; idempotencyKey: string } | null>(null)
  const current = listing.breadcrumbs.at(-1)
  const back = current?.id === request.directoryId ? listing.breadcrumbs.at(-2) : current
  const ready = listing.status === 'ready' && current?.id === request.directoryId
  const folders = listing.items.filter(item => item.type === 'DIR' && item.id !== entry.id)

  function selectDirectory(directoryId: string) {
    if (inFlight.current) return
    setMoveError('')
    openDirectory(directoryId)
  }

  async function submit() {
    // 2. 确认时提交打开窗口时的源版本、已选目标和幂等键。
    if (!ready || inFlight.current) return
    const targetParentId = request.directoryId
    inFlight.current = true
    setSubmitting(true)
    setMoveError('')

    try {
      const previous = lastAttempt.current
      const attempt = previous?.targetParentId === targetParentId ? previous : {
        targetParentId, idempotencyKey: crypto.randomUUID(),
      }
      lastAttempt.current = attempt
      await moveEntry({ entryId: entry.id, expectedRevision: entry.revision, ...attempt })

      // 3. 成功后通知原目录重新读取；失败时保留当前选择。
      onSaved()
    } catch (error) {
      const message = apiErrorMessage(error)
      setMoveError(error instanceof ApiError && error.status === 401 ? `${message}。请重新加载页面后再提交。` : message)
    } finally {
      inFlight.current = false
      setSubmitting(false)
    }
  }

  return (
    <Modal title="移动条目" open onOk={() => void submit()}
      onCancel={() => { if (!inFlight.current) onClose() }}
      confirmLoading={submitting} closable={!submitting} keyboard={!submitting}
      okButtonProps={{ disabled: !ready || submitting }} cancelButtonProps={{ disabled: submitting }}
      okText="移动到此处" cancelText="取消" width={440}>
      <div className="move-entry-form">
        <p>正在移动「{entry.name}」</p>
        {current && <p aria-label="目标目录路径">{listing.breadcrumbs.map(crumb => crumb.name || '根目录').join(' / ')}</p>}
        {back && <Button disabled={submitting} onClick={() => selectDirectory(back.id)}>返回</Button>}
        {folders.length > 0 && (
          <ul className="move-entry-folders" aria-label="目标文件夹">
            {folders.map(folder => (
              <li key={folder.id}>
                <button type="button" className="directory-link" disabled={submitting}
                  onClick={() => selectDirectory(folder.id)}>{folder.name}</button>
              </li>
            ))}
          </ul>
        )}
        {listing.status === 'loading' && <p role="status">正在读取目标目录…</p>}
        {ready && folders.length === 0 && <p role="status">本页没有可选子文件夹。</p>}
        {ready && listing.nextCursor !== null && (
          <Button disabled={submitting} onClick={() => requestPage(listing.nextCursor)}>加载更多目标文件夹</Button>
        )}
        {listing.status === 'error' && (
          <>
            <p className="auth-error" role="alert">{listing.errorMessage}</p>
            <Button onClick={() => requestPage(request.cursor)}>重试读取目标目录</Button>
          </>
        )}
        {moveError && <p className="auth-error" role="alert">{moveError}</p>}
      </div>
    </Modal>
  )
}
