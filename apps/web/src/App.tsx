import { useEffect, useRef, useState } from 'react'
import type { ChangeEvent, DragEvent } from 'react'
import { App as AntApp, Button, Checkbox, Dropdown, Input, Modal, Select, Tooltip } from 'antd'
import type { MenuProps } from 'antd'
import {
  ArrowDown, ArrowUpDown,
  Bell, Check, CheckCheck, ChevronDown, ChevronRight, Clock3,
  CloudUpload, Download, Ellipsis, File, FileArchive, FileAudio, FileImage,
  FileText, FileVideo, Folder, FolderInput, FolderPlus, Grid2X2, HardDrive,
  Info, LayoutList, Menu, Monitor, Pause, Play, RotateCcw, Search,
  ShieldCheck, Smartphone, Star, Trash2, Upload, X,
} from 'lucide-react'
import {
  createDemoState, formatDate, formatSize, isAvailable, kindFromName, uniqueName,
} from './demo'
import type { DemoState, DriveEntry, FileKind, Section, Transfer } from './demo'

const sectionLabels: Record<Section, string> = {
  files: '我的文件', recent: '最近使用', starred: '我的收藏', transfers: '传输列表', trash: '回收站',
}
const fileFilters: { key: 'all' | FileKind; label: string }[] = [
  { key: 'all', label: '全部' }, { key: 'document', label: '文档' },
  { key: 'image', label: '图片' }, { key: 'video', label: '视频' },
  { key: 'audio', label: '音频' }, { key: 'archive', label: '压缩包' },
]
const kindLabels: Record<FileKind, string> = {
  folder: '文件夹', document: '文档', image: '图片', video: '视频', audio: '音频', archive: '压缩包', other: '其他文件',
}
const navItems = [
  { key: 'files' as Section, icon: Folder, label: '我的文件' },
  { key: 'recent' as Section, icon: Clock3, label: '最近使用' },
  { key: 'starred' as Section, icon: Star, label: '我的收藏' },
  { key: 'transfers' as Section, icon: ArrowUpDown, label: '传输列表' },
  { key: 'trash' as Section, icon: Trash2, label: '回收站' },
]

function expandIds(ids: string[], entries: DriveEntry[]) {
  const expanded = new Set(ids)
  let changed = true
  while (changed) {
    changed = false
    entries.forEach(entry => {
      if (entry.parentId && expanded.has(entry.parentId) && !expanded.has(entry.id)) {
        expanded.add(entry.id)
        changed = true
      }
    })
  }
  return expanded
}

function FileVisual({ entry, large = false }: { entry: DriveEntry; large?: boolean }) {
  const Icon = {
    folder: Folder, document: FileText, image: FileImage, video: FileVideo,
    audio: FileAudio, archive: FileArchive, other: File,
  }[entry.kind]
  const extension = entry.name.split('.').pop()?.toLowerCase() ?? ''
  if (entry.kind === 'image' && entry.preview) {
    return <span className={'file-visual image-visual' + (large ? ' large' : '')}><img src={entry.preview} alt="" /></span>
  }
  return <span className={['file-visual', entry.kind, extension === 'pdf' ? 'pdf' : '', extension === 'fig' ? 'fig' : '', large ? 'large' : ''].join(' ')}>
    <Icon size={large ? 46 : 22} strokeWidth={1.65} />
    {entry.kind !== 'folder' && <small>{extension.toUpperCase().slice(0, 4)}</small>}
  </span>
}

function TransferList({ transfers, onAction }: {
  transfers: Transfer[]
  onAction: (id: string, action: 'toggle' | 'cancel' | 'remove') => void
}) {
  const [tab, setTab] = useState<'all' | 'active' | 'completed'>('all')
  const shown = transfers.filter(task => tab === 'all' || (tab === 'active' ? ['running', 'paused'].includes(task.status) : task.status === 'completed'))
  const labels = { running: '正在传输', paused: '已暂停', completed: '已完成', cancelled: '已取消', failed: '传输未完成' }
  return <div className="transfer-content">
    <div className="transfer-tabs">
      {([{ key: 'all', label: '全部任务' }, { key: 'active', label: '进行中' }, { key: 'completed', label: '已完成' }] as const).map(item =>
        <button key={item.key} className={tab === item.key ? 'active' : ''} onClick={() => setTab(item.key)}>{item.label}</button>,
      )}
    </div>
    {!shown.length && <div className="small-empty"><CloudUpload size={38} /><h3>这里还没有传输任务</h3><p>上传一份文件，就能看到它的旅程。</p></div>}
    {shown.map(task => <div className="transfer-item" key={task.id}>
      <FileVisual entry={task.entry} />
      <div className="transfer-main">
        <strong>{task.entry.name}</strong>
        <div className="transfer-meta"><span>{task.direction === 'upload' ? '上传' : '下载'} · {formatSize(task.entry.size)}</span><span className={task.status}>{labels[task.status]}</span></div>
        {['running', 'paused'].includes(task.status) && <div className="transfer-progress"><span style={{ width: task.progress + '%' }} /></div>}
      </div>
      <div className="transfer-controls">
        {['running', 'paused'].includes(task.status) ? <>
          <button className="icon-button" aria-label={(task.status === 'running' ? '暂停 ' : '继续 ') + task.entry.name} onClick={() => onAction(task.id, 'toggle')}>
            {task.status === 'running' ? <Pause size={16} /> : <Play size={16} />}
          </button>
          <button className="icon-button" aria-label={'取消 ' + task.entry.name} onClick={() => onAction(task.id, 'cancel')}><X size={16} /></button>
        </> : task.status === 'completed' ? <CheckCheck size={19} className="green-text" /> :
          <button className="icon-button" aria-label={'移除任务 ' + task.entry.name} onClick={() => onAction(task.id, 'remove')}><X size={16} /></button>}
      </div>
    </div>)}
    <p className="transfer-note"><ShieldCheck size={14} />当前为传输体验，文件不会发送到服务器。</p>
  </div>
}

export default function App() {
  const { message, modal } = AntApp.useApp()
  const [state, setState] = useState<DemoState>(createDemoState)
  const [section, setSection] = useState<Section>('files')
  const [folderId, setFolderId] = useState<string | null>(null)
  const [query, setQuery] = useState('')
  const [filter, setFilter] = useState<'all' | FileKind>('all')
  const [view, setView] = useState<'list' | 'grid'>('list')
  const [sort, setSort] = useState('modified')
  const [selected, setSelected] = useState<string[]>([])
  const [mobileNav, setMobileNav] = useState(false)
  const [dragging, setDragging] = useState(false)
  const [nameDialog, setNameDialog] = useState<{ kind: 'create' | 'rename'; entry?: DriveEntry } | null>(null)
  const [nameValue, setNameValue] = useState('')
  const [nameError, setNameError] = useState('')
  const [moveIds, setMoveIds] = useState<string[]>([])
  const [moveParent, setMoveParent] = useState('__root__')
  const [previewId, setPreviewId] = useState<string | null>(null)
  const [previewText, setPreviewText] = useState('')
  const [showTransfers, setShowTransfers] = useState(false)
  const uploadInput = useRef<HTMLInputElement>(null)
  const searchInput = useRef<HTMLInputElement>(null)
  const objectUrls = useRef(new Set<string>())
  const dragDepth = useRef(0)

  const activeEntries = state.entries.filter(entry => isAvailable(entry, state.entries))
  const currentFolder = state.entries.find(entry => entry.id === folderId)
  const previewEntry = state.entries.find(entry => entry.id === previewId)
  const runningCount = state.transfers.filter(task => ['running', 'paused'].includes(task.status)).length
  const hasRunningTransfers = state.transfers.some(task => task.status === 'running')
  const trashEntries = state.entries.filter(entry => entry.deletedAt)
  const usedBytes = state.entries.reduce((total, entry) => total + entry.size, 0)
  const activeFolders = activeEntries.filter(entry => entry.kind === 'folder')
  const showQuickFolders = section === 'files' && !folderId && !query && filter === 'all'
  const quickFolders = activeFolders.filter(entry => !entry.parentId)
  const heading = query ? '搜索结果' : section === 'files' && currentFolder ? currentFolder.name : sectionLabels[section]

  const breadcrumbs: DriveEntry[] = []
  let cursor = currentFolder
  while (cursor && breadcrumbs.length < 30) {
    breadcrumbs.unshift(cursor)
    cursor = state.entries.find(entry => entry.id === cursor?.parentId)
  }

  let entries = section === 'trash' ? trashEntries : section === 'starred' ? activeEntries.filter(entry => entry.starred) :
    section === 'recent' ? activeEntries.filter(entry => entry.kind !== 'folder') :
    query ? activeEntries : activeEntries.filter(entry => entry.parentId === folderId)
  entries = entries.filter(entry =>
    (!query || entry.name.toLocaleLowerCase().includes(query.toLocaleLowerCase())) &&
    (filter === 'all' || entry.kind === filter) &&
    (!showQuickFolders || entry.kind !== 'folder'),
  ).sort((left, right) => {
    if (left.kind === 'folder' && right.kind !== 'folder') return -1
    if (left.kind !== 'folder' && right.kind === 'folder') return 1
    return sort === 'name' ? left.name.localeCompare(right.name, 'zh-CN') :
      sort === 'size' ? right.size - left.size : (right.deletedAt ?? right.modifiedAt) - (left.deletedAt ?? left.modifiedAt)
  })

  useEffect(() => {
    if (!hasRunningTransfers) return
    const timer = window.setInterval(() => {
      setState(previous => {
        let nextEntries = previous.entries
        const nextTransfers = previous.transfers.map(task => {
          if (task.status !== 'running') return task
          const progress = Math.min(task.progress + 7, 100)
          if (progress < 100) return { ...task, progress }
          const parent = previous.entries.find(entry => entry.id === task.entry.parentId)
          if (task.direction === 'upload' && task.entry.parentId && (!parent || !isAvailable(parent, previous.entries))) {
            return { ...task, progress, status: 'failed' as const }
          }
          if (task.direction === 'upload' && !nextEntries.some(entry => entry.id === task.entry.id)) {
            nextEntries = [...nextEntries, task.entry]
          }
          return { ...task, progress: 100, status: 'completed' as const }
        })
        return { entries: nextEntries, transfers: nextTransfers }
      })
    }, 240)
    return () => window.clearInterval(timer)
  }, [hasRunningTransfers])

  useEffect(() => {
    setSelected([])
  }, [section, folderId, filter, query])

  useEffect(() => {
    if (folderId && (!currentFolder || !isAvailable(currentFolder, state.entries))) setFolderId(null)
  }, [folderId, currentFolder, state.entries])

  useEffect(() => {
    let cancelled = false
    setPreviewText(previewEntry?.content ?? '')
    if (previewEntry?.file && /\.(txt|md|csv|json)$/i.test(previewEntry.name) && previewEntry.size <= 2 * 1024 * 1024) {
      previewEntry.file.text().then(text => { if (!cancelled) setPreviewText(text) }).catch(() => { if (!cancelled) setPreviewText('暂时无法读取这个文件，可以下载后查看。') })
    }
    return () => { cancelled = true }
  }, [previewEntry])

  useEffect(() => {
    const keyListener = (event: KeyboardEvent) => {
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k') {
        event.preventDefault()
        searchInput.current?.focus()
      }
      if (event.key === 'Escape') setMobileNav(false)
    }
    window.addEventListener('keydown', keyListener)
    const urls = objectUrls.current
    return () => {
      window.removeEventListener('keydown', keyListener)
      urls.forEach(url => URL.revokeObjectURL(url))
    }
  }, [])

  function navigate(next: Section, nextFolder: string | null = null) {
    setSection(next); setFolderId(nextFolder); setQuery(''); setFilter('all'); setMobileNav(false)
  }

  function openEntry(entry: DriveEntry) {
    if (entry.kind === 'folder') navigate('files', entry.id)
    else setPreviewId(entry.id)
  }

  function toggleStar(ids: string[]) {
    const allStarred = state.entries.filter(entry => ids.includes(entry.id)).every(entry => entry.starred)
    setState(previous => ({ ...previous, entries: previous.entries.map(entry => ids.includes(entry.id) ? { ...entry, starred: !allStarred } : entry) }))
    message.success(allStarred ? '已取消收藏' : '已加入我的收藏')
  }

  function restoreEntries(ids: string[]) {
    setState(previous => {
      let restored = previous.entries
      ids.forEach(id => {
        const target = restored.find(entry => entry.id === id)
        if (!target) return
        const parent = restored.find(entry => entry.id === target.parentId)
        const parentId = parent && isAvailable(parent, restored) ? parent.id : null
        const name = uniqueName(target.name, restored.filter(entry => entry.id !== id && entry.parentId === parentId && !entry.deletedAt))
        restored = restored.map(entry => entry.id === id ? { ...entry, parentId, name, deletedAt: null } : entry)
      })
      return { ...previous, entries: restored }
    })
    setSelected([])
    message.success('已恢复文件，名称冲突时会自动添加后缀')
  }

  function trash(ids: string[]) {
    setState(previous => ({ ...previous, entries: previous.entries.map(entry => ids.includes(entry.id) ? { ...entry, deletedAt: Date.now() } : entry) }))
    setSelected([])
    message.open({ type: 'success', content: <span>已移入回收站 <button className="toast-link" onClick={() => restoreEntries(ids)}>撤销</button></span> })
  }

  function purge(ids: string[]) {
    modal.confirm({
      title: ids.length === trashEntries.length ? '清空回收站？' : '永久删除所选文件？',
      content: '这些文件将从本次体验中移除，删除后无法恢复。',
      okText: '永久删除', okButtonProps: { danger: true }, cancelText: '再想想',
      onOk: () => {
        const removeIds = expandIds(ids, state.entries)
        setState(previous => ({ ...previous, entries: previous.entries.filter(entry => !removeIds.has(entry.id)) }))
        setSelected([]); message.success('已永久删除')
      },
    })
  }

  function startNameDialog(kind: 'create' | 'rename', entry?: DriveEntry) {
    setNameDialog({ kind, entry }); setNameValue(entry?.name ?? ''); setNameError('')
  }

  function submitName() {
    if (!nameDialog) return
    const name = nameValue.trim().normalize('NFC')
    if (!name || /[<>:"/\\|?*\u0000-\u001f]/.test(name) || /[. ]$/.test(name)) {
      setNameError('请输入有效名称，不包含 / \\ : * ? 等字符'); return
    }
    const parentId = nameDialog.kind === 'rename' ? nameDialog.entry!.parentId : folderId
    if (state.entries.some(entry => entry.id !== nameDialog.entry?.id && entry.parentId === parentId && !entry.deletedAt && entry.name.toLocaleLowerCase() === name.toLocaleLowerCase())) {
      setNameError('这个位置已经有同名文件，请换一个名称'); return
    }
    if (nameDialog.kind === 'create') {
      const entry: DriveEntry = { id: crypto.randomUUID(), name, parentId, kind: 'folder', size: 0, modifiedAt: Date.now(), device: 'Windows 电脑', starred: false, deletedAt: null, color: 'green' }
      setState(previous => ({ ...previous, entries: [...previous.entries, entry] }))
      if (section !== 'files') navigate('files', folderId)
      message.success('文件夹已创建')
    } else {
      setState(previous => ({ ...previous, entries: previous.entries.map(entry => entry.id === nameDialog.entry!.id ? { ...entry, name, modifiedAt: Date.now() } : entry) }))
      message.success('已重命名')
    }
    setNameDialog(null)
  }

  function moveSelected() {
    const parentId = moveParent === '__root__' ? null : moveParent
    const moving = state.entries.filter(entry => moveIds.includes(entry.id))
    const reservedNames = state.entries.filter(entry => entry.parentId === parentId && !entry.deletedAt && !moveIds.includes(entry.id)).map(entry => entry.name.toLocaleLowerCase())
    if (moving.some(entry => reservedNames.includes(entry.name.toLocaleLowerCase())) || new Set(moving.map(entry => entry.name.toLocaleLowerCase())).size !== moving.length) {
      message.warning('目标文件夹有同名文件，请先重命名'); return
    }
    setState(previous => ({ ...previous, entries: previous.entries.map(entry => moveIds.includes(entry.id) ? { ...entry, parentId, modifiedAt: Date.now() } : entry) }))
    setMoveIds([]); setSelected([]); message.success('文件已移动')
  }

  function addUploads(files: FileList | File[]) {
    if (!files.length) return
    const targetParent = section === 'files' ? folderId : null
    const newFiles = Array.from(files)
    const reserved = [...state.entries, ...state.transfers.filter(task => !['cancelled', 'failed'].includes(task.status)).map(task => task.entry)].filter(entry => entry.parentId === targetParent && !entry.deletedAt)
      const tasks = newFiles.map(file => {
        const kind = kindFromName(file.name)
        const preview = ['image', 'video', 'audio'].includes(kind) ? URL.createObjectURL(file) : undefined
        if (preview) objectUrls.current.add(preview)
        const entry: DriveEntry = {
          id: crypto.randomUUID(), parentId: targetParent, name: uniqueName(file.name, reserved), kind,
          size: file.size, modifiedAt: Date.now(), device: 'Windows 电脑', starred: false, deletedAt: null, file, preview,
        }
        reserved.push(entry)
        return { id: crypto.randomUUID(), entry, direction: 'upload' as const, progress: 0, status: 'running' as const }
      })
    setState(previous => ({ ...previous, transfers: [...tasks, ...previous.transfers] }))
    setShowTransfers(true)
    message.success('已添加 ' + newFiles.length + ' 个上传任务')
  }

  function onFileChange(event: ChangeEvent<HTMLInputElement>) {
    if (event.target.files) addUploads(event.target.files)
    event.target.value = ''
  }

  function download(entriesToDownload: DriveEntry[]) {
    const files = entriesToDownload.filter(entry => entry.kind !== 'folder')
    if (!files.length) return
    const tasks: Transfer[] = files.map(entry => ({ id: crypto.randomUUID(), entry, direction: 'download', progress: 0, status: 'running' }))
    setState(previous => ({ ...previous, transfers: [...tasks, ...previous.transfers] }))
    files.forEach(async entry => {
      let blob: Blob
      let name = entry.name
      try {
        if (entry.file) blob = entry.file
        else if (entry.preview) blob = await (await fetch(entry.preview)).blob()
        else {
          blob = new Blob([entry.content ?? 'CloudNest 体验文件\n\n文件名称：' + entry.name + '\n\n这是一份演示说明，用于体验下载流程。'], { type: 'text/plain;charset=utf-8' })
          if (!entry.content) name += '.演示说明.txt'
        }
        const url = URL.createObjectURL(blob)
        const anchor = document.createElement('a')
        anchor.href = url; anchor.download = name; anchor.click()
        window.setTimeout(() => URL.revokeObjectURL(url), 1000)
      } catch {
        const taskId = tasks.find(task => task.entry.id === entry.id)?.id
        setState(previous => ({ ...previous, transfers: previous.transfers.map(task => task.id === taskId ? { ...task, status: 'failed' } : task) }))
        message.error('文件暂时无法下载，请稍后重试')
      }
    })
    message.success('已开始下载' + (files.some(entry => !entry.file && !entry.content && !entry.preview) ? '，示例文件将下载为演示说明' : ''))
  }

  function transferAction(id: string, action: 'toggle' | 'cancel' | 'remove') {
    setState(previous => ({ ...previous, transfers: action === 'remove' ? previous.transfers.filter(task => task.id !== id) :
      previous.transfers.map(task => task.id === id ? { ...task, status: action === 'cancel' ? 'cancelled' : task.status === 'paused' ? 'running' : 'paused' } : task) }))
  }

  function entryMenu(entry: DriveEntry): MenuProps {
    return { items: section === 'trash' ? [
      { key: 'restore', icon: <RotateCcw size={16} />, label: '恢复文件' },
      { key: 'purge', icon: <Trash2 size={16} />, label: '永久删除', danger: true },
    ] : [
      { key: 'open', icon: entry.kind === 'folder' ? <Folder size={16} /> : <FileImage size={16} />, label: entry.kind === 'folder' ? '打开文件夹' : '预览文件' },
      ...(entry.kind !== 'folder' ? [{ key: 'download', icon: <Download size={16} />, label: '下载' }] : []),
      { key: 'star', icon: <Star size={16} />, label: entry.starred ? '取消收藏' : '加入收藏' },
      { type: 'divider' },
      { key: 'rename', icon: <FileText size={16} />, label: '重命名' },
      { key: 'move', icon: <FolderInput size={16} />, label: '移动到' },
      { type: 'divider' },
      { key: 'trash', icon: <Trash2 size={16} />, label: '移入回收站', danger: true },
    ], onClick: ({ key }) => {
      if (key === 'open') openEntry(entry)
      if (key === 'download') download([entry])
      if (key === 'star') toggleStar([entry.id])
      if (key === 'rename') startNameDialog('rename', entry)
      if (key === 'move') { setMoveIds([entry.id]); setMoveParent(entry.parentId ?? '__root__') }
      if (key === 'trash') trash([entry.id])
      if (key === 'restore') restoreEntries([entry.id])
      if (key === 'purge') purge([entry.id])
    } }
  }

  function dropFiles(event: DragEvent) {
    event.preventDefault(); dragDepth.current = 0; setDragging(false)
    if (event.dataTransfer.files.length) addUploads(event.dataTransfer.files)
  }

  const selectedEntries = state.entries.filter(entry => selected.includes(entry.id))
  const disabledFolders = expandIds(moveIds.filter(id => state.entries.find(entry => entry.id === id)?.kind === 'folder'), state.entries)
  const sortLabels: Record<string, string> = { modified: '最近修改', name: '文件名称', size: '文件大小' }

  return <div className="drive-app" onDragEnter={event => {
    if (!event.dataTransfer.types.includes('Files')) return
    event.preventDefault(); dragDepth.current++; setDragging(true)
  }} onDragOver={event => { if (event.dataTransfer.types.includes('Files')) event.preventDefault() }}
  onDragLeave={event => { event.preventDefault(); dragDepth.current--; if (dragDepth.current <= 0) { dragDepth.current = 0; setDragging(false) } }}
  onDrop={dropFiles}>
    {mobileNav && <button className="sidebar-backdrop" aria-label="关闭导航" onClick={() => setMobileNav(false)} />}
    <aside className={'sidebar' + (mobileNav ? ' is-open' : '')}>
      <button className="brand" aria-label="CloudNest 首页" onClick={() => navigate('files')}>
        <img src="/favicon.svg" alt="" /><span>CloudNest<small>让文件，自在流动</small></span>
      </button>
      <div className="workspace-label">个人空间<span>PERSONAL</span></div>
      <nav aria-label="主要导航">
        {navItems.map(item => <button key={item.key} className={'nav-item' + (section === item.key ? ' active' : '')} onClick={() => navigate(item.key)} title={item.label}>
          <item.icon size={19} strokeWidth={1.7} /><span>{item.label}</span>
          {item.key === 'transfers' && runningCount > 0 && <b className="nav-badge">{runningCount}</b>}
          {item.key === 'trash' && trashEntries.length > 0 && <small>{trashEntries.length}</small>}
        </button>)}
      </nav>
      <div className="device-section">
        <div className="sidebar-section-label">我的设备<span>3</span></div>
        {[
          { icon: Monitor, title: 'Windows 电脑', subtitle: '当前设备', current: true },
          { icon: Smartphone, title: '华为 Pura 70 Pro+', subtitle: '最近在线', current: false },
          { icon: Grid2X2, title: '小米平板', subtitle: '最近在线', current: false },
        ].map(device => <button className="device" key={device.title} onClick={() => modal.info({
          title: device.title, content: <div className="info-copy"><p>这是预置的设备示例，帮你体验多设备文件归集。</p><p>你可以在文件列表中查看每个文件来自哪台设备。</p></div>, okText: '知道了',
        })}><span className="device-icon"><device.icon size={17} /></span><span><strong>{device.title}</strong><small>{device.subtitle}</small></span><i className={device.current ? 'online' : ''} /></button>)}
      </div>
      <div className="sidebar-bottom">
        <div className="storage-card">
          <div><HardDrive size={17} /><span>存储空间</span><span className="storage-total">100 GB</span></div>
          <div className="storage-track"><span style={{ width: Math.min(usedBytes / (100 * 1024 ** 3) * 100, 100) + '%' }} /></div>
          <p>已使用 <strong>{formatSize(usedBytes)}</strong> / 100 GB</p>
          <small><ShieldCheck size={12} />每一份重要，都妥善安放</small>
        </div>
        <Dropdown trigger={['click']} menu={{ items: [
          { key: 'about', label: '关于本次体验', icon: <Info size={16} /> },
          { key: 'reset', label: '重新开始体验', icon: <RotateCcw size={16} /> },
        ], onClick: ({ key }) => {
          if (key === 'about') modal.info({ title: '欢迎来到 CloudNest', content: '你可以放心试试上传、整理和删除。所有操作仅在本次页面会话中生效，刷新后恢复初始示例；所选文件不会上传到服务器。', okText: '开始体验' })
          else modal.confirm({ title: '重新开始体验？', content: '本次添加的文件和操作会被清空，恢复最初的示例。', okText: '重置体验', cancelText: '保留当前', onOk: () => {
            objectUrls.current.forEach(url => URL.revokeObjectURL(url)); objectUrls.current.clear()
            setState(createDemoState()); setPreviewId(null); setShowTransfers(false); navigate('files')
          } })
        } }}>
          <button className="profile"><span className="avatar">R</span><span><strong>root</strong><small>我的个人云盘</small></span><ChevronDown size={15} /></button>
        </Dropdown>
      </div>
    </aside>

    <div className="main-shell">
      <header className="topbar">
        <div className="topbar-left"><button className="icon-button mobile-menu" aria-label="打开导航" onClick={() => setMobileNav(true)}><Menu size={21} /></button><span className="space-title">我的空间</span><span className="topbar-slash">/</span><span className="topbar-current">{sectionLabels[section]}</span></div>
        <div className="topbar-actions">
          <label className="search-box"><Search size={17} /><input ref={searchInput} placeholder="搜索文件、文件夹…" aria-label="搜索文件、文件夹" value={query} onChange={event => { if (section === 'transfers') navigate('files'); setQuery(event.target.value) }} />
            {query ? <button aria-label="清除搜索" onClick={() => setQuery('')}><X size={14} /></button> : <kbd>Ctrl K</kbd>}
          </label>
          <Tooltip title="操作仅在本次会话生效，刷新后重置"><span className="demo-badge"><span />体验版</span></Tooltip>
          <Tooltip title="传输任务"><button className="icon-button transfer-trigger" aria-label="查看传输任务" onClick={() => setShowTransfers(true)}><ArrowUpDown size={19} />{runningCount > 0 && <i>{runningCount}</i>}</button></Tooltip>
          <button className="icon-button notification-button" aria-label="查看通知" onClick={() => modal.info({ title: '一切都准备好了', content: <div className="notification-copy"><span className="notification-icon"><Check size={24} /></span><p>欢迎来到你的个人文件空间。</p><p>试试新建一个文件夹，或者把文件拖进来。</p></div>, okText: '去试试' })}><Bell size={19} /><i /></button>
          <span className="top-avatar">R</span>
        </div>
      </header>

      <main className="main-content">
        <div className="page-heading">
          <div><div className="eyebrow"><span />YOUR PERSONAL CLOUD</div><h1>{heading}<span className="heading-dot">.</span></h1><p>{section === 'trash' ? '误删也没关系，重要的文件还可以找回来。' : section === 'transfers' ? '每一份文件的旅程，都清清楚楚。' : section === 'starred' ? '把重要的放在眼前，把喜欢的留在手边。' : query ? '在你的空间里，找到需要的那一份。' : '生活、灵感与热爱，都有自己的位置。'}</p></div>
          <div className="heading-actions">
            {section === 'trash' ? <button className="button secondary" disabled={!trashEntries.length} onClick={() => purge(trashEntries.map(entry => entry.id))}><Trash2 size={17} />清空回收站</button> : section === 'transfers' ? <button className="button secondary" onClick={() => setState(previous => ({ ...previous, transfers: previous.transfers.filter(task => task.status !== 'completed') }))}><CheckCheck size={17} />清除已完成</button> : <>
              <button className="button secondary" onClick={() => startNameDialog('create')}><FolderPlus size={17} />新建文件夹</button>
              <button className="button primary" onClick={() => uploadInput.current?.click()}><Upload size={17} />上传文件<ChevronDown size={13} /></button>
            </>}
          </div>
        </div>

        {section === 'transfers' ? <section className="transfer-page surface"><TransferList transfers={state.transfers} onAction={transferAction} /></section> : <>
          {showQuickFolders && <section className="quick-section" aria-label="快捷文件夹">
            <div className="section-heading"><h2>快捷访问</h2><span>给生活一点秩序</span></div>
            <div className="quick-folders">{quickFolders.map(folder => <article className={'folder-card ' + (folder.color ?? 'green')} key={folder.id}>
              <button className="folder-main" aria-label={'打开文件夹 ' + folder.name} onClick={() => openEntry(folder)}><span className="folder-art"><Folder size={41} strokeWidth={1.3} fill="currentColor" /></span><span className="folder-copy"><strong>{folder.name}</strong><small>{activeEntries.filter(entry => entry.parentId === folder.id).length} 个项目</small></span></button>
              <Dropdown trigger={['click']} menu={entryMenu(folder)}><button className="icon-button folder-menu" aria-label={folder.name + ' 更多操作'}><Ellipsis size={18} /></button></Dropdown>
            </article>)}</div>
          </section>}

          <section className="files-section surface">
            <div className="files-toolbar">
              {selected.length ? <div className="selection-toolbar"><span>已选择 <b>{selected.length}</b> 项</span>
                {section === 'trash' ? <><button onClick={() => restoreEntries(selected)}><RotateCcw size={16} />恢复</button><button className="danger-text" onClick={() => purge(selected)}><Trash2 size={16} />永久删除</button></> :
                  <><button disabled={selectedEntries.every(entry => entry.kind === 'folder')} onClick={() => download(selectedEntries)}><Download size={16} />下载</button><button onClick={() => { setMoveIds(selected); setMoveParent('__root__') }}><FolderInput size={16} />移动</button><button onClick={() => toggleStar(selected)}><Star size={16} />收藏</button><button className="danger-text" onClick={() => trash(selected)}><Trash2 size={16} />删除</button></>}
                <button className="selection-close" aria-label="取消选择" onClick={() => setSelected([])}><X size={16} /></button>
              </div> : <div className="file-filters" aria-label="文件类型筛选">{fileFilters.map(item => <button key={item.key} className={filter === item.key ? 'active' : ''} onClick={() => setFilter(item.key)}>{item.label}</button>)}</div>}
              {!selected.length && <div className="list-tools">
                <Dropdown trigger={['click']} menu={{ items: Object.entries(sortLabels).map(([key, label]) => ({ key, label, icon: sort === key ? <Check size={14} /> : <span className="menu-spacer" /> })), onClick: ({ key }) => setSort(key) }}><button className="sort-button" aria-label="排序方式"><ArrowDown size={14} /><span>{sortLabels[sort]}</span><ChevronDown size={12} /></button></Dropdown>
                <div className="view-switch"><button aria-label="列表视图" aria-pressed={view === 'list'} className={view === 'list' ? 'active' : ''} onClick={() => setView('list')}><LayoutList size={17} /></button><button aria-label="网格视图" aria-pressed={view === 'grid'} className={view === 'grid' ? 'active' : ''} onClick={() => setView('grid')}><Grid2X2 size={16} /></button></div>
              </div>}
            </div>

            <div className="location-bar"><div className="breadcrumbs"><button onClick={() => navigate(section)}><HardDrive size={14} />{section === 'files' ? '我的文件' : sectionLabels[section]}</button>{section === 'files' && breadcrumbs.map(crumb => <span key={crumb.id}><ChevronRight size={13} /><button onClick={() => navigate('files', crumb.id)}>{crumb.name}</button></span>)}{query && <span><ChevronRight size={13} />“{query}”</span>}</div><span>{entries.length} 个项目</span></div>

            {!entries.length ? <div className="empty-state"><div className="empty-icon">{query ? <Search size={34} /> : section === 'trash' ? <Trash2 size={34} /> : section === 'starred' ? <Star size={34} /> : <Folder size={34} />}</div><h3>{query ? '还没有找到匹配的文件' : section === 'trash' ? '回收站空空的' : section === 'starred' ? '把喜欢的文件收藏起来' : '这个文件夹，等待你的第一份收藏'}</h3><p>{query ? '换一个关键词，或试试其他文件类型。' : section === 'trash' ? '让重要的文件，都好好留在身边。' : section === 'starred' ? '点击文件旁的星标，就能在这里快速找到它。' : '上传文件，或者新建文件夹慢慢整理。'}</p>{!query && section === 'files' && <button className="button primary" onClick={() => uploadInput.current?.click()}><Upload size={16} />上传文件</button>}</div> :
              view === 'list' ? <div className="table-wrap"><table className="file-table"><thead><tr>
                <th className="check-cell"><Checkbox aria-label="全选文件" checked={entries.length > 0 && entries.every(entry => selected.includes(entry.id))} indeterminate={selected.length > 0 && !entries.every(entry => selected.includes(entry.id))} onChange={event => setSelected(event.target.checked ? entries.map(entry => entry.id) : [])} /></th>
                <th>文件名称 <ChevronDown size={12} /></th><th className="date-column">{section === 'trash' ? '删除时间' : '修改时间'}</th><th className="size-column">文件大小</th><th className="device-column">来自设备</th><th className="action-cell"><span className="sr-only">操作</span></th>
              </tr></thead><tbody>{entries.map(entry => <tr key={entry.id} className={selected.includes(entry.id) ? 'selected' : ''}>
                <td className="check-cell"><Checkbox aria-label={'选择 ' + entry.name} checked={selected.includes(entry.id)} onChange={event => setSelected(previous => event.target.checked ? [...previous, entry.id] : previous.filter(id => id !== entry.id))} /></td>
                <td><div className="file-name-cell"><button className="file-open" onClick={() => section === 'trash' ? undefined : openEntry(entry)} disabled={section === 'trash'}><FileVisual entry={entry} /><span><strong>{entry.name}</strong><small>{kindLabels[entry.kind]}</small></span></button>{entry.starred && section !== 'trash' && <Star className="inline-star" size={13} fill="currentColor" />}</div></td>
                <td className="date-column muted-text">{formatDate(entry.deletedAt ?? entry.modifiedAt)}</td>
                <td className="size-column muted-text">{entry.kind === 'folder' ? '—' : formatSize(entry.size)}</td>
                <td className="device-column"><span className={'device-tag ' + (entry.device === '华为手机' ? 'phone' : entry.device === '小米平板' ? 'tablet' : '')}>{entry.device === 'Windows 电脑' ? <Monitor size={12} /> : entry.device === '华为手机' ? <Smartphone size={12} /> : <Grid2X2 size={11} />}{entry.device}</span></td>
                <td className="action-cell"><div className="row-actions">{section !== 'trash' ? <button className={'icon-button star-button' + (entry.starred ? ' is-starred' : '')} aria-label={(entry.starred ? '取消收藏 ' : '收藏 ') + entry.name} onClick={() => toggleStar([entry.id])}><Star size={16} fill={entry.starred ? 'currentColor' : 'none'} /></button> : <button className="icon-button" aria-label={'恢复 ' + entry.name} onClick={() => restoreEntries([entry.id])}><RotateCcw size={16} /></button>}<Dropdown trigger={['click']} menu={entryMenu(entry)}><button className="icon-button" aria-label={entry.name + ' 更多操作'}><Ellipsis size={19} /></button></Dropdown></div></td>
              </tr>)}</tbody></table></div> :
              <div className="file-grid">{entries.map(entry => <article className={'file-grid-card' + (selected.includes(entry.id) ? ' selected' : '')} key={entry.id}>
                <div className="grid-card-tools"><Checkbox aria-label={'选择 ' + entry.name} checked={selected.includes(entry.id)} onChange={event => setSelected(previous => event.target.checked ? [...previous, entry.id] : previous.filter(id => id !== entry.id))} /><Dropdown trigger={['click']} menu={entryMenu(entry)}><button className="icon-button" aria-label={entry.name + ' 更多操作'}><Ellipsis size={19} /></button></Dropdown></div>
                <button className={'grid-preview ' + entry.kind} disabled={section === 'trash'} aria-label={'打开 ' + entry.name} onClick={() => openEntry(entry)}>{entry.preview ? <img src={entry.preview} alt="" /> : <div className={'grid-file-art ' + entry.kind}><FileVisual entry={entry} large />{entry.kind === 'document' && <div className="paper-lines"><i /><i /><i /></div>}</div>}</button>
                <div className="grid-card-info"><button disabled={section === 'trash'} onClick={() => openEntry(entry)}>{entry.name}</button><p>{formatSize(entry.size)}<span>{formatDate(entry.modifiedAt).split(' ')[0]}</span></p></div>
              </article>)}</div>}
            <div className="file-list-footer"><span><ShieldCheck size={13} />属于你的空间，安心存放每一份日常</span><span>CloudNest · Make room for what matters.</span></div>
          </section>
          <div className="drop-hint"><CloudUpload size={16} /><span>也可以把文件拖到这里，轻松开始上传</span></div>
        </>}
        <footer className="page-footer"><span>少一点寻找，多一点专注。</span><span>Made for your everyday <span className="footer-spark">✧</span></span></footer>
      </main>
    </div>

    <input className="sr-only" type="file" multiple ref={uploadInput} onChange={onFileChange} aria-label="选择上传文件" />
    {dragging && <div className="drop-overlay"><div><CloudUpload size={58} strokeWidth={1.4} /><h2>松开鼠标，把文件放进来</h2><p>文件将添加到{currentFolder?.name ?? '我的文件'}</p></div></div>}
    <Modal title={nameDialog?.kind === 'rename' ? '给文件换个名字' : '新建文件夹'} open={!!nameDialog} onCancel={() => setNameDialog(null)} onOk={submitName} okText={nameDialog?.kind === 'rename' ? '保存名称' : '创建文件夹'} cancelText="取消" width={420} destroyOnHidden>
      <div className="name-dialog"><label htmlFor="entry-name">名称</label><Input id="entry-name" autoFocus value={nameValue} maxLength={120} placeholder={nameDialog?.kind === 'rename' ? '输入新名称' : '比如：新的灵感'} status={nameError ? 'error' : undefined} onChange={event => { setNameValue(event.target.value); setNameError('') }} onPressEnter={submitName} />{nameError && <p role="alert" className="form-error">{nameError}</p>}<small>保存位置：{currentFolder?.name ?? '我的文件'}</small></div>
    </Modal>
    <Modal title="移动到文件夹" open={moveIds.length > 0} onCancel={() => setMoveIds([])} onOk={moveSelected} okText="移动到这里" cancelText="取消" width={440}>
      <div className="name-dialog"><p>为所选 {moveIds.length} 个项目选择一个新位置。</p><Select aria-label="目标文件夹" value={moveParent} onChange={setMoveParent} style={{ width: '100%' }} options={[{ value: '__root__', label: '我的文件（根目录）' }, ...activeFolders.filter(folder => !disabledFolders.has(folder.id)).map(folder => ({ value: folder.id, label: folder.name }))]} /></div>
    </Modal>
    <Modal open={!!previewEntry} onCancel={() => setPreviewId(null)} width={780} centered title={<span className="preview-title">{previewEntry?.name}</span>} footer={previewEntry ? <div className="preview-footer"><span>{formatSize(previewEntry.size)} · {previewEntry.device}</span><div><Button icon={<Star size={15} />} onClick={() => toggleStar([previewEntry.id])}>{previewEntry.starred ? '取消收藏' : '收藏'}</Button><Button type="primary" icon={<Download size={15} />} onClick={() => download([previewEntry])}>下载文件</Button></div></div> : null} destroyOnHidden>
      {previewEntry && <div className={'preview-body ' + previewEntry.kind}>
        {previewEntry.kind === 'image' && previewEntry.preview ? <img className="preview-image" src={previewEntry.preview} alt={previewEntry.name} /> :
          previewEntry.kind === 'video' && previewEntry.file && previewEntry.preview ? <video controls src={previewEntry.preview} className="preview-video" /> :
          previewEntry.kind === 'audio' && previewEntry.file && previewEntry.preview ? <div className="audio-preview"><FileAudio size={60} /><audio controls src={previewEntry.preview} /></div> :
          previewText ? <pre className="text-preview">{previewText}</pre> :
          previewEntry.kind === 'document' ? <div className="document-preview"><span>CLOUDNEST / PERSONAL NOTES</span><h2>{previewEntry.name.replace(/\.[^.]+$/, '')}</h2><div className="document-rule" /><h3>让每一份文件，都有归处。</h3><p>生活里的灵感、学习时的收获，还有值得记住的日常。<br />把它们放在喜欢的位置，需要的时候，随时都能找到。</p><h3>从这里，开始你的文件旅程</h3><p>上传、预览、收藏，再放进合适的文件夹。<br />简单一点，也安心一点。</p><small>这是一份示例预览，用于体验文件阅读。</small></div> :
          <div className="generic-preview"><FileVisual entry={previewEntry} large /><h3>{previewEntry.name}</h3><p>{previewEntry.file ? '这个格式暂不支持在线预览，可以下载原文件查看。' : '这是一个示例文件，可以体验收藏、移动和下载流程。'}</p></div>}
      </div>}
    </Modal>
    <Modal title={<span className="transfer-modal-title"><ArrowUpDown size={19} />传输列表{runningCount > 0 && <small>{runningCount} 个任务进行中</small>}</span>} open={showTransfers} onCancel={() => setShowTransfers(false)} footer={null} width={560} destroyOnHidden>
      <TransferList transfers={state.transfers} onAction={transferAction} />
    </Modal>
  </div>
}
