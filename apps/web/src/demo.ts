export type FileKind = 'folder' | 'document' | 'image' | 'video' | 'audio' | 'archive' | 'other'
export type Section = 'files' | 'recent' | 'starred' | 'transfers' | 'trash'
export type Device = 'Windows 电脑' | '华为手机' | '小米平板'

export interface DriveEntry {
  id: string
  parentId: string | null
  name: string
  kind: FileKind
  size: number
  modifiedAt: number
  device: Device
  starred: boolean
  deletedAt: number | null
  color?: string
  preview?: string
  content?: string
  file?: File
}

export interface Transfer {
  id: string
  entry: DriveEntry
  direction: 'upload' | 'download'
  progress: number
  status: 'running' | 'paused' | 'completed' | 'cancelled' | 'failed'
}

export interface DemoState {
  entries: DriveEntry[]
  transfers: Transfer[]
}

const MB = 1024 * 1024
const GB = 1024 * MB
const time = (day: number, hour = 15, minute = 30) => new Date(2026, 10, day, hour, minute).getTime()

const makeFile = (
  id: string, name: string, kind: FileKind, size: number,
  parentId: string | null = null, extra: Partial<DriveEntry> = {},
): DriveEntry => ({
  id, name, kind, size, parentId, modifiedAt: time(3),
  device: 'Windows 电脑', starred: false, deletedAt: null, ...extra,
})

export function createDemoState(): DemoState {
  const entries: DriveEntry[] = [
    ...[
      ['design', '设计素材', 'sand'],
      ['study', '学习笔记', 'blue'],
      ['life', '生活记录', 'pink'],
      ['project', '项目文档', 'green'],
    ].map(([id, name, color]) => makeFile(id, name, 'folder', 0, null, { color })),
    makeFile('readme', 'CloudNest 使用指南.pdf', 'document', 3.8 * MB, null, { modifiedAt: time(3, 16, 42), starred: true }),
    makeFile('requirements', '我的云盘 · 产品需求.md', 'document', 87 * 1024, null, { modifiedAt: time(3, 15, 28), starred: true, content: '# 我的云盘 · 产品需求\n\n让每一份文件，都有归处。\n\n## 我想做的事\n\n- 手机和电脑之间，轻松传文件\n- 用文件夹整理学习、工作和生活\n- 重要的文件，一键加入收藏\n- 删除后还能从回收站找回来\n\n## 体验记录\n\n先让常用的操作变得顺手，再慢慢完善更多功能。' }),
    makeFile('forest', '秋日山野.svg', 'image', 1.7 * MB, null, { modifiedAt: time(3, 14, 16), device: '华为手机', preview: '/assets/forest.svg', starred: true }),
    makeFile('coast', '微风海岸.svg', 'image', 2.4 * MB, null, { modifiedAt: time(3, 11, 52), device: '华为手机', preview: '/assets/coast.svg' }),
    makeFile('components', 'CloudNest 组件探索.fig', 'other', 24.6 * MB, null, { modifiedAt: time(2, 21, 10) }),
    makeFile('trip', '把秋天装进口袋.mp4', 'video', 8.6 * GB, null, { modifiedAt: time(2, 19, 38), device: '华为手机' }),
    makeFile('sound', '雨天的白噪音.wav', 'audio', 18.2 * MB, null, { modifiedAt: time(2, 17, 5), device: '小米平板' }),
    makeFile('archive', '上学期学习资料.zip', 'archive', 1.6 * GB, null, { modifiedAt: time(1, 10, 20) }),
    makeFile('weekly', '十一月学习计划.xlsx', 'document', 128 * 1024, null, { modifiedAt: time(1, 9, 0), device: '小米平板' }),
    makeFile('design-ui', '云盘界面设计.fig', 'other', 58 * MB, 'design', { starred: true }),
    makeFile('design-colors', '配色与灵感记录.md', 'document', 2400, 'design', { content: '# 配色与灵感\n\n主色：薄荷绿。\n\n干净的留白、柔和的边界，让文件成为主角。' }),
    makeFile('design-image', '山间插画.svg', 'image', 4800, 'design', { preview: '/assets/forest.svg' }),
    makeFile('study-go', 'Go 并发学习笔记.md', 'document', 94 * 1024, 'study', { starred: true, device: '小米平板', content: '# Go 并发学习笔记\n\n## 今天的收获\n\nGoroutine 让并发任务的表达更自然。\nChannel 用来在任务之间传递数据。\n\n## 接下来想练习\n\n- 写一个有界并发的任务队列\n- 给长任务增加取消能力\n- 记录错误，并安全地重试\n\n慢一点，把每个小实验做扎实。' }),
    makeFile('study-pdf', '分布式系统阅读材料.pdf', 'document', 8.2 * MB, 'study'),
    makeFile('study-plan', '这周要学什么.txt', 'document', 1200, 'study', { content: '这周要学什么\n\n周一：Go 的文件与流\n周二：React 组件和状态\n周三：对象存储入门\n周四：整理和复习\n周末：出去走走，拍一点照片。' }),
    makeFile('life-video', '和朋友去露营.mp4', 'video', 2.6 * GB, 'life', { device: '华为手机' }),
    makeFile('life-list', '周末出行清单.md', 'document', 3200, 'life', { content: '# 周末出行清单\n\n- 相机和充电器\n- 雨伞\n- 喜欢的零食\n- 一点点好心情' }),
    makeFile('project-plan', '云盘技术方案.md', 'document', 146 * 1024, 'project', { starred: true, content: '# CloudNest 技术方案\n\n先做一个自己喜欢用的文件云盘。\n\n## 第一站\n\n体验文件浏览、上传、预览、整理和回收站。\n\n## 后续旅程\n\n可靠传输、多端使用，以及安全的文件同步。' }),
    makeFile('project-api', '接口设计草稿.md', 'document', 22 * 1024, 'project', { content: '# 接口设计草稿\n\n这是一份页面体验用的示例文档。\n\n先理解用户要做什么，再让每个接口的职责清楚。' }),
    makeFile('trash-note', '旧版学习计划.md', 'document', 12 * 1024, null, { deletedAt: time(2, 10), modifiedAt: time(1) }),
    makeFile('trash-design', '未采用的设计稿.fig', 'other', 6.8 * MB, 'design', { deletedAt: time(1, 12), modifiedAt: time(1) }),
  ]
  return {
    entries,
    transfers: [
      { id: 'demo-transfer-1', entry: entries.find(entry => entry.id === 'forest')!, direction: 'upload', progress: 100, status: 'completed' },
      { id: 'demo-transfer-2', entry: entries.find(entry => entry.id === 'requirements')!, direction: 'upload', progress: 100, status: 'completed' },
    ],
  }
}

export function isAvailable(entry: DriveEntry, entries: DriveEntry[]): boolean {
  if (entry.deletedAt) return false
  let parentId = entry.parentId
  const visited = new Set<string>()
  while (parentId) {
    if (visited.has(parentId)) return false
    visited.add(parentId)
    const parent = entries.find(candidate => candidate.id === parentId)
    if (!parent || parent.deletedAt) return false
    parentId = parent.parentId
  }
  return true
}

export function uniqueName(name: string, siblings: DriveEntry[]): string {
  const names = new Set(siblings.map(entry => entry.name.toLocaleLowerCase()))
  if (!names.has(name.toLocaleLowerCase())) return name
  const dot = name.lastIndexOf('.')
  const stem = dot > 0 ? name.slice(0, dot) : name
  const extension = dot > 0 ? name.slice(dot) : ''
  let suffix = 1
  while (names.has((stem + ' (' + suffix + ')' + extension).toLocaleLowerCase())) suffix++
  return stem + ' (' + suffix + ')' + extension
}

export function kindFromName(name: string): FileKind {
  const extension = name.split('.').pop()?.toLowerCase() ?? ''
  if (['jpg', 'jpeg', 'png', 'gif', 'webp', 'avif', 'svg'].includes(extension)) return 'image'
  if (['mp4', 'mov', 'webm', 'mkv'].includes(extension)) return 'video'
  if (['mp3', 'wav', 'flac', 'm4a', 'ogg'].includes(extension)) return 'audio'
  if (['zip', 'rar', '7z', 'tar', 'gz'].includes(extension)) return 'archive'
  if (['pdf', 'md', 'txt', 'doc', 'docx', 'xls', 'xlsx', 'ppt', 'pptx', 'csv', 'json'].includes(extension)) return 'document'
  return 'other'
}

export function formatSize(bytes: number): string {
  if (!bytes) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB']
  const exponent = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), 3)
  return (bytes / 1024 ** exponent).toFixed(exponent > 1 ? 1 : 0) + ' ' + units[exponent]
}

export function formatDate(timestamp: number): string {
  return new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false }).format(timestamp).replaceAll('/', '-')
}
