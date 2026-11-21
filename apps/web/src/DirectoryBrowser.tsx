import { useEffect, useRef, useState } from 'react'
import type { MouseEvent } from 'react'
import { Button, Input, Modal } from 'antd'
import type { DirectoryCreator, DirectoryEntry, DirectoryPageReader, EntryMover, EntryRenamer, EntryTrasher } from './api/drive'
import type { PublishedUploadEntryReader, SingleUploadContentSender, SingleUploadTaskCreator, SingleUploadTaskReader } from './api/uploadTask'
import type { FileAccessAuthorizer, FileAccessGrant, FileAccessPurpose } from './api/download'
import { isRasterPreviewMimeType } from './api/download'
import { ApiError, apiErrorMessage } from './api/http'
import { formatSize } from './demo'
import EntryNameDialog from './EntryNameDialog'
import MoveEntryDialog from './MoveEntryDialog'
import EntryConfirmationDialog from './EntryConfirmationDialog'
import SingleUploadDialog from './SingleUploadDialog'
import RasterImagePreview from './RasterImagePreview'
import { useDirectoryListing } from './useDirectoryListing'

type EntryAction = { kind: 'create' | 'select-upload' } | { kind: 'rename' | 'move' | 'trash'; entry: DirectoryEntry }
type FileAccessState = { entry: DirectoryEntry; purpose: FileAccessPurpose } & (
  | { phase: 'loading' }
  | { phase: 'ready'; grant: FileAccessGrant }
  | { phase: 'error'; message: string; retryPurpose: FileAccessPurpose | null }
)

export default function DirectoryBrowser({
  rootEntryId, readDirectoryPage, createDirectory, createUploadTask, sendUploadContent, readUploadTask, readPublishedEntry,
  authorizeFileAccess, renameEntry, moveEntry, trashEntry,
}: {
  rootEntryId: string
  readDirectoryPage: DirectoryPageReader
  createDirectory: DirectoryCreator
  createUploadTask: SingleUploadTaskCreator
  sendUploadContent: SingleUploadContentSender
  readUploadTask: SingleUploadTaskReader
  readPublishedEntry: PublishedUploadEntryReader
  authorizeFileAccess: FileAccessAuthorizer
  renameEntry: EntryRenamer
  moveEntry: EntryMover
  trashEntry: EntryTrasher
}) {
  const { request, listing, requestPage, openDirectory } = useDirectoryListing(rootEntryId, readDirectoryPage)
  const [entryAction, setEntryAction] = useState<EntryAction | null>(null)
  const [fileAccess, setFileAccess] = useState<FileAccessState | null>(null)
  const accessRequest = useRef<AbortController | null>(null)

  useEffect(() => () => accessRequest.current?.abort(), [])

  // 1. 切换目录时从首页加载，清除上一目录的条目和分页游标。
  function navigateToDirectory(directoryId: string) {
    if (directoryId === request.directoryId) return
    setEntryAction(null)
    closeFileAccess()
    openDirectory(directoryId)
  }

  // 2. 使用服务端确认的目录路径生成当前位置和上级入口。
  const isRoot = request.directoryId === rootEntryId
  const directoryLabel = isRoot ? '根目录' : '文件夹'
  const currentDirectory = listing.breadcrumbs.at(-1)

  // 1. 打开名称表单时记住待修改条目及其版本。
  const renameTarget = entryAction?.kind === 'rename' ? entryAction.entry : null
  const trashTarget = entryAction?.kind === 'trash' ? entryAction.entry : null
  const canModify = currentDirectory?.id === request.directoryId && listing.status === 'ready'
  const accessLabel = fileAccess?.purpose === 'preview' ? '预览' : '下载'
  const retryPurpose = fileAccess?.phase === 'error' ? fileAccess.retryPurpose : null

  // 2. 确认成功后从当前目录的首页重新读取。
  function refreshAfterChange() {
    setEntryAction(null)
    requestPage(null)
  }

  async function requestFileAccess(entry: DirectoryEntry, purpose: FileAccessPurpose) {
    // 1. 保存本次文件版本和用途，挡住并发申请。
    if (entry.type !== 'FILE' || accessRequest.current) return
    const controller = new AbortController()
    accessRequest.current = controller
    setFileAccess({ entry, purpose, phase: 'loading' })
    try {
      // 2. 仅在当前弹窗保存短期链接和有效期。
      const grant = await authorizeFileAccess(entry.id, entry.revision, purpose, controller.signal)
      if (!controller.signal.aborted) setFileAccess({ entry, purpose, phase: 'ready', grant })
    } catch (error) {
      // 3. 版本冲突时刷新目录。不支持预览时，等待用户手动改为下载。
      if (controller.signal.aborted) return
      const revisionChanged = error instanceof ApiError && error.code === 'REVISION_CONFLICT'
      const previewUnsupported = purpose === 'preview' && error instanceof ApiError && error.code === 'UNSUPPORTED_PREVIEW'
      const message = revisionChanged ? '文件版本已变化。请关闭窗口，从更新后的列表重新获取链接。' :
        previewUnsupported ? '此文件暂不支持预览，可以改为下载。' : apiErrorMessage(error)
      setFileAccess({ entry, purpose, phase: 'error', message,
        retryPurpose: revisionChanged ? null : previewUnsupported ? 'download' : purpose })
      if (revisionChanged) requestPage(null)
    } finally {
      if (accessRequest.current === controller) accessRequest.current = null
    }
  }

  function closeFileAccess() {
    accessRequest.current?.abort()
    accessRequest.current = null
    setFileAccess(null)
  }

  function followDownloadLink(event: MouseEvent<HTMLElement>) {
    // 1. 下载开始前复查有效期，有效时由浏览器使用原签名链接保存附件。
    if (fileAccess?.phase !== 'ready' || fileAccess.purpose !== 'download') {
      event.preventDefault()
      return
    }
    if (fileAccess.grant.expiresAt > Date.now()) return

    // 2. 过期时取消本次导航，保留文件信息供用户重新申请授权。
    event.preventDefault()
    setFileAccess({ entry: fileAccess.entry, purpose: 'download', phase: 'error',
      message: '下载链接已过期，请重新获取。', retryPurpose: 'download' })
  }

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
                    <button type="button" className="directory-link" onClick={() => navigateToDirectory(crumb.id)}>
                      {label}
                    </button>
                  )}
                </li>
              )
            })}
          </ol>
        </nav>
      )}
      {currentDirectory?.id === request.directoryId && (
        <div className="directory-toolbar">
          <Button disabled={!canModify} onClick={() => setEntryAction({ kind: 'create' })}>新建文件夹</Button>
          <Button disabled={!canModify} onClick={() => setEntryAction({ kind: 'select-upload' })}>选择上传文件</Button>
        </div>
      )}
      {entryAction?.kind === 'select-upload' && currentDirectory?.id === request.directoryId && (
        <SingleUploadDialog parentId={currentDirectory.id}
          targetPath={listing.breadcrumbs.map(crumb => crumb.name || '根目录').join(' / ')}
          createUploadTask={createUploadTask} sendUploadContent={sendUploadContent} readUploadTask={readUploadTask}
          readPublishedEntry={readPublishedEntry} onPublishedEntryRead={() => requestPage(null)}
          onClose={() => setEntryAction(null)} />
      )}
      {(entryAction?.kind === 'create' || entryAction?.kind === 'rename') && currentDirectory?.id === request.directoryId && (
        <EntryNameDialog title={renameTarget ? '重命名' : '新建文件夹'}
          inputLabel={renameTarget ? '新名称' : '文件夹名称'}
          confirmLabel={renameTarget ? '保存名称' : '创建文件夹'}
          hint={renameTarget ? `原名称为「${renameTarget.name}」。` : `将在「${currentDirectory.name || '根目录'}」中创建。`}
          initialName={renameTarget?.name ?? ''}
          submitName={(name, idempotencyKey) => renameTarget ? renameEntry({
            entryId: renameTarget.id, expectedRevision: renameTarget.revision, name, idempotencyKey,
          }) : createDirectory({ parentId: currentDirectory.id, name, idempotencyKey })}
          onClose={() => setEntryAction(null)} onSaved={refreshAfterChange} />
      )}
      {entryAction?.kind === 'move' && (
        <MoveEntryDialog entry={entryAction.entry} rootEntryId={rootEntryId}
          readDirectoryPage={readDirectoryPage} moveEntry={moveEntry}
          onClose={() => setEntryAction(null)} onSaved={refreshAfterChange} />
      )}
      {trashTarget && (
        <EntryConfirmationDialog title="确认移入回收站" message={`将「${trashTarget.name}」移入回收站？`}
          confirmText="移入回收站" danger onClose={() => setEntryAction(null)}
          onConfirm={async idempotencyKey => {
            await trashEntry({ entryId: trashTarget.id, expectedRevision: trashTarget.revision, idempotencyKey })
            refreshAfterChange()
          }} />
      )}
      {fileAccess && (
        <Modal title={`${accessLabel}授权`} open onCancel={closeFileAccess} width={480}
          footer={(
            <>
              <Button onClick={closeFileAccess}>关闭</Button>
              {retryPurpose && (
                <Button onClick={() => void requestFileAccess(fileAccess.entry, retryPurpose)}>
                  {retryPurpose === fileAccess.purpose ? '重新获取链接' : '改为获取下载链接'}
                </Button>
              )}
              {fileAccess.phase === 'ready' && fileAccess.purpose === 'download' && (
                <Button type="primary" href={fileAccess.grant.url} target="_blank" rel="noopener noreferrer"
                  onClick={followDownloadLink}>下载文件</Button>
              )}
            </>
          )}>
          <p style={{ overflowWrap: 'anywhere' }}>「{fileAccess.entry.name}」</p>
          {fileAccess.phase === 'loading' && <p role="status">正在获取短期{accessLabel}链接…</p>}
          {fileAccess.phase === 'error' && <p className="auth-error" role="alert">{fileAccess.message}</p>}
          {fileAccess.phase === 'ready' && (
            <div className="entry-name-form">
              <label htmlFor="authorized-file-url">短期{accessLabel}链接</label>
              <Input.TextArea id="authorized-file-url" readOnly autoSize={{ minRows: 2, maxRows: 4 }} value={fileAccess.grant.url} />
              <small>有效期至 <time dateTime={new Date(fileAccess.grant.expiresAt).toISOString()}>
                {new Date(fileAccess.grant.expiresAt).toLocaleString('zh-CN', { hour12: false })}
              </time></small>
              {fileAccess.purpose === 'preview' && isRasterPreviewMimeType(fileAccess.grant.previewMimeType) && (
                <RasterImagePreview filename={fileAccess.entry.name} grant={fileAccess.grant}
                  onRefresh={() => void requestFileAccess(fileAccess.entry, 'preview')} />
              )}
            </div>
          )}
        </Modal>
      )}
      {listing.items.length > 0 && (
        <table className="directory-items" aria-label={`${directoryLabel}条目`}>
          <thead><tr><th scope="col">名称</th><th scope="col">类型</th><th scope="col">大小</th><th scope="col">操作</th></tr></thead>
          <tbody>
            {listing.items.map(item => (
              <tr key={item.id}>
                <td>{item.type === 'DIR' ? (
                  <button type="button" className="directory-link" onClick={() => navigateToDirectory(item.id)}>
                    {item.name}
                  </button>
                ) : item.name}</td>
                <td>{item.type === 'DIR' ? '文件夹' : '文件'}</td>
                <td>{item.type === 'DIR' ? '—' : formatSize(item.sizeBytes)}</td>
                <td>
                  <div className="directory-actions">
                    {item.type === 'FILE' && (
                      <>
                        <button type="button" className="directory-link" aria-label={`获取预览链接 ${item.name}`}
                          disabled={!canModify} onClick={() => void requestFileAccess(item, 'preview')}>获取预览链接</button>
                        <button type="button" className="directory-link" aria-label={`获取下载链接 ${item.name}`}
                          disabled={!canModify} onClick={() => void requestFileAccess(item, 'download')}>获取下载链接</button>
                      </>
                    )}
                    <button type="button" className="directory-link" aria-label={`重命名 ${item.name}`}
                      disabled={!canModify} onClick={() => setEntryAction({ kind: 'rename', entry: item })}>重命名</button>
                    <button type="button" className="directory-link" aria-label={`移动 ${item.name}`}
                      disabled={!canModify} onClick={() => setEntryAction({ kind: 'move', entry: item })}>移动</button>
                    <button type="button" className="directory-link" aria-label={`将 ${item.name} 移入回收站`}
                      disabled={!canModify} onClick={() => setEntryAction({ kind: 'trash', entry: item })}>移入回收站</button>
                  </div>
                </td>
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
