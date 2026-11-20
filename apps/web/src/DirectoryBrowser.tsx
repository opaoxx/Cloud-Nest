import { useState } from 'react'
import { Button } from 'antd'
import type { DirectoryCreator, DirectoryEntry, DirectoryPageReader, EntryMover, EntryRenamer, EntryTrasher } from './api/drive'
import { formatSize } from './demo'
import EntryNameDialog from './EntryNameDialog'
import MoveEntryDialog from './MoveEntryDialog'
import EntryConfirmationDialog from './EntryConfirmationDialog'
import { useDirectoryListing } from './useDirectoryListing'

type EntryAction = { kind: 'create' } | { kind: 'rename' | 'move' | 'trash'; entry: DirectoryEntry }

export default function DirectoryBrowser({ rootEntryId, readDirectoryPage, createDirectory, renameEntry, moveEntry, trashEntry }: {
  rootEntryId: string
  readDirectoryPage: DirectoryPageReader
  createDirectory: DirectoryCreator
  renameEntry: EntryRenamer
  moveEntry: EntryMover
  trashEntry: EntryTrasher
}) {
  const { request, listing, requestPage, openDirectory } = useDirectoryListing(rootEntryId, readDirectoryPage)
  const [entryAction, setEntryAction] = useState<EntryAction | null>(null)

  // 1. 切换目录时从首页加载，清除上一目录的条目和分页游标。
  function navigateToDirectory(directoryId: string) {
    if (directoryId === request.directoryId) return
    setEntryAction(null)
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

  // 2. 确认成功后从当前目录的首页重新读取。
  function refreshAfterChange() {
    setEntryAction(null)
    requestPage(null)
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
        <Button className="create-directory-trigger" disabled={!canModify}
          onClick={() => setEntryAction({ kind: 'create' })}>新建文件夹</Button>
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
