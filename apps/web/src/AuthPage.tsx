import { useCallback, useEffect, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { Button, Input } from 'antd'
import { getCurrentUser, login, logoutWebSession, refreshWebSession } from './api/auth'
import type { CurrentUserProfile } from './api/auth'
import { confirmPurge, createDirectory, listTrashPage, moveEntry, readDirectoryPageWithRefresh, readPurgePreview, readPurgeProgress, renameEntry, restoreEntry, trashEntry } from './api/drive'
import type { DirectoryCreator, DirectoryPageReader, EntryMover, EntryRenamer, EntryRestorer, EntryTrasher, PurgeConfirmer, PurgePreviewReader, PurgeProgressReader, TrashPageReader } from './api/drive'
import { ApiError, apiErrorMessage } from './api/http'
import { createFileAccessGrant } from './api/download'
import type { FileAccessAuthorizer } from './api/download'
import { cancelSingleUploadTask, createSingleUploadTask, readPublishedUploadEntry, readSingleUploadTask, sendSingleUploadContent } from './api/uploadTask'
import type {
  PublishedUploadEntryReader, SingleUploadContentSender, SingleUploadTaskCanceller, SingleUploadTaskCreator, SingleUploadTaskReader,
} from './api/uploadTask'
import DirectoryBrowser from './DirectoryBrowser'
import TrashBrowser from './TrashBrowser'
import { formatSize } from './demo'
import './auth.css'

export default function AuthPage() {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const activeSession = useRef<{ accessToken: string } | null>(null)
  const logoutInFlight = useRef(false)
  const profileReadVersion = useRef(0)
  const [profile, setProfile] = useState<CurrentUserProfile | null>(null)
  const [view, setView] = useState<'files' | 'trash'>('files')
  const [restoring, setRestoring] = useState(true)
  const [busy, setBusy] = useState<'login' | 'profile' | 'logout' | null>(null)
  const [errorMessage, setErrorMessage] = useState('')

  const clearSession = useCallback((error?: unknown) => {
    activeSession.current = null
    setProfile(null)
    setView('files')
    setErrorMessage(error === undefined ? '' : apiErrorMessage(error))
  }, [])

  const readDirectoryPage = useCallback<DirectoryPageReader>(async (parentId, cursor, signal) => {
    // 1. 读取当前内存会话，并用它发起目录只读请求。
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    const requestedToken = session.accessToken

    const renewAccessToken = async () => {
      // 2. 共享已有刷新请求，令牌更新后由当前会话继续使用。
      if (activeSession.current !== session) throw new ApiError(401, '请登录后重试。')
      if (session.accessToken !== requestedToken) return session.accessToken
      if (logoutInFlight.current) throw new ApiError(0, '正在退出，请稍后再试。')
      try {
        const renewedToken = await refreshWebSession()
        if (activeSession.current !== session) throw new ApiError(401, '请登录后重试。')
        session.accessToken = renewedToken
        return renewedToken
      } catch (error) {
        if (activeSession.current === session && !logoutInFlight.current) clearSession(error)
        throw error
      }
    }

    try {
      return await readDirectoryPageWithRefresh(requestedToken, parentId, cursor, signal, renewAccessToken)
    } catch (error) {
      // 3. 列表返回其他 401 或重试仍返回 401 时，清理本地会话。
      if (!signal.aborted && activeSession.current === session && !logoutInFlight.current &&
          error instanceof ApiError && error.status === 401) {
        clearSession(error)
      }
      throw error
    }
  }, [clearSession])

  const readTrashPage = useCallback<TrashPageReader>(async (cursor, signal) => {
    // 使用当前内存令牌执行回收站 GET，读取失败交给列表提示。
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    return listTrashPage(session.accessToken, cursor, signal)
  }, [])

  const readPreview = useCallback<PurgePreviewReader>(async (entryId, signal) => {
    // 使用当前会话读取清理范围，预检窗口处理读取失败。
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    return readPurgePreview(session.accessToken, entryId, signal)
  }, [])

  const readProgress = useCallback<PurgeProgressReader>(async (receipt, signal) => {
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    return readPurgeProgress(session.accessToken, receipt, signal)
  }, [])

  const submitDirectoryCreation: DirectoryCreator = async creation => {
    // 从当前内存会话取令牌，写请求直接提交一次。
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    await createDirectory(session.accessToken, creation)
  }

  const submitUploadTask: SingleUploadTaskCreator = async (declaration, idempotencyKey) => {
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    return createSingleUploadTask(session.accessToken, declaration, idempotencyKey)
  }

  const submitUploadContent: SingleUploadContentSender = async (receipt, file) => {
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    await sendSingleUploadContent(session.accessToken, receipt, file)
  }

  const readUploadTask: SingleUploadTaskReader = async (taskId, signal) => {
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    return readSingleUploadTask(session.accessToken, taskId, signal)
  }

  const cancelUploadTask: SingleUploadTaskCanceller = async taskId => {
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    return cancelSingleUploadTask(session.accessToken, taskId)
  }

  const readPublishedEntry: PublishedUploadEntryReader = async (entryId, signal) => {
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    return readPublishedUploadEntry(session.accessToken, entryId, signal)
  }

  const authorizeFileAccess: FileAccessAuthorizer = async (entryId, revision, purpose, signal) => {
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    return createFileAccessGrant(session.accessToken, entryId, revision, purpose, signal)
  }

  const submitEntryRename: EntryRenamer = async rename => {
    // 1. 使用此刻的内存令牌，直接提交经用户确认的重命名。
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    await renameEntry(session.accessToken, rename)
  }

  const submitEntryMove: EntryMover = async move => {
    // 使用当前内存令牌直接提交一次经用户确认的移动。
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    await moveEntry(session.accessToken, move)
  }

  const submitEntryTrash: EntryTrasher = async trash => {
    // 使用当前内存令牌直接提交一次经用户确认的软删除。
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    await trashEntry(session.accessToken, trash)
  }

  const submitEntryRestore: EntryRestorer = async restore => {
    // 使用当前内存令牌直接提交一次经用户确认的恢复。
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    return restoreEntry(session.accessToken, restore)
  }

  const submitPurgeConfirmation: PurgeConfirmer = async (preview, idempotencyKey) => {
    // 使用当前内存令牌交付确认，沿用现有写请求的单次提交规则。
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    return confirmPurge(session.accessToken, preview, idempotencyKey)
  }

  useEffect(() => {
    let active = true

    async function restoreSession() {
      try {
        // 1. 页面加载时通过刷新接口尝试恢复会话。
        const accessToken = await refreshWebSession()
        if (!active) return

        // 2. 用新的访问令牌读取本人资料，两步都成功后写入内存会话。
        const profile = await getCurrentUser(accessToken)
        if (active) {
          activeSession.current = { accessToken }
          setProfile(profile)
        }
      } catch (error) {
        // 3. 收到 401 时回到登录页，其他错误显示提示。
        if (active && !(error instanceof ApiError && error.status === 401)) {
          setErrorMessage(apiErrorMessage(error))
        }
      } finally {
        if (active) setRestoring(false)
      }
    }

    void restoreSession()
    return () => {
      // 页面离开时忽略结果，让已送达后端的刷新请求完成 Cookie 交付。
      active = false
    }
  }, [])

  async function submitLogin(event: FormEvent<HTMLFormElement>) {
    // 1. 取出本次凭据，清空输入框并开始登录。
    event.preventDefault()
    if (busy) return
    setBusy('login')
    setErrorMessage('')
    const submittedPassword = password
    setPassword('')
    try {
      // 2. 登录后带 Access Token 读取本人资料，再展示已认证页面。
      const accessToken = await login(username, submittedPassword)
      const profile = await getCurrentUser(accessToken)
      activeSession.current = { accessToken }
      setProfile(profile)
    } catch (error) {
      // 3. 登录或资料读取失败时，交付可重试的错误提示。
      setErrorMessage(apiErrorMessage(error))
    } finally {
      setBusy(null)
    }
  }

  async function refreshProfile(signal?: AbortSignal) {
    // 1. 从当前会话只读获取最新资料，记录本次读取的顺序。
    const session = activeSession.current
    if (!session) throw new ApiError(401, '请登录后重试。')
    const version = ++profileReadVersion.current
    const isCurrent = () => !signal?.aborted && activeSession.current === session && profileReadVersion.current === version

    try {
      const nextProfile = await getCurrentUser(session.accessToken, signal)

      // 2. 防止已关闭窗口或较早发出的请求覆盖当前资料。
      if (isCurrent()) setProfile(nextProfile)
    } catch (error) {
      if (isCurrent()) throw error
    }
  }

  async function submitLogout() {
    // 1. 记录当前会话，通过同步标记拦住连点和新的会话刷新。
    const session = activeSession.current
    if (!session || busy || logoutInFlight.current) return
    const requestedToken = session.accessToken
    logoutInFlight.current = true
    setBusy('logout')
    setErrorMessage('')
    try {
      // 2. 服务确认撤销后清理本页会话，再显示登录表单。
      await logoutWebSession(requestedToken)
      if (activeSession.current === session) clearSession()
    } catch (error) {
      // 3. 失败时保留当前会话。令牌过期时引导恢复会话后手动退出。
      if (activeSession.current !== session) return
      const message = error instanceof ApiError && error.code === 'TOKEN_EXPIRED' && session.accessToken === requestedToken
        ? '访问令牌已过期，请重新加载页面后再退出。'
        : apiErrorMessage(error)
      setErrorMessage(`无法确认退出。${message}`)
    } finally {
      logoutInFlight.current = false
      setBusy(null)
    }
  }

  async function readProfileAgain() {
    // 1. 用当前内存会话重新读取资料。
    const session = activeSession.current
    if (!session || busy) return
    const requestedToken = session.accessToken
    setBusy('profile')
    setErrorMessage('')
    try {
      await refreshProfile()
    } catch (error) {
      // 2. 401 表明需要重新登录，其他失败保留已显示资料。
      if (activeSession.current !== session || session.accessToken !== requestedToken) return
      if (error instanceof ApiError && error.status === 401) clearSession(error)
      else setErrorMessage(apiErrorMessage(error))
    } finally {
      setBusy(null)
    }
  }

  return (
    <main className="auth-shell">
      <section className={profile ? 'auth-card auth-card--signed-in' : 'auth-card'} aria-labelledby="auth-title">
        <div className="auth-brand"><img src="/favicon.svg" alt="" />CloudNest</div>
        <h1 id="auth-title">
          {restoring ? <span role="status">正在检查会话…</span> : profile ? '本人资料' : '登录'}
        </h1>
        {errorMessage && <p className="auth-error" role="alert">{errorMessage}</p>}
        {!restoring && (profile ? (
          <>
            <dl className="auth-profile">
              <div><dt>显示名称</dt><dd>{profile.user.display_name}</dd></div>
              <div><dt>用户名</dt><dd>{profile.user.username}</dd></div>
              <div><dt>总配额</dt><dd>{formatSize(profile.quota_bytes)}</dd></div>
              <div><dt>已用空间</dt><dd>{formatSize(profile.used_bytes)}</dd></div>
              <div><dt>预占空间</dt><dd>{formatSize(profile.reserved_bytes)}</dd></div>
            </dl>
            <div className="auth-actions">
              <Button loading={busy === 'profile'} disabled={busy !== null} onClick={() => void readProfileAgain()}>重新读取资料</Button>
              <Button disabled={busy !== null} onClick={() => void submitLogout()}>
                {busy === 'logout' ? '正在退出…' : '退出登录'}
              </Button>
            </div>
            <nav className="drive-view-nav" aria-label="文件视图">
              <Button type={view === 'files' ? 'primary' : 'default'} aria-pressed={view === 'files'}
                onClick={() => setView('files')}>我的文件</Button>
              <Button type={view === 'trash' ? 'primary' : 'default'} aria-pressed={view === 'trash'}
                onClick={() => setView('trash')}>回收站</Button>
            </nav>
            <div hidden={view !== 'files'}>
              <DirectoryBrowser key={profile.root_entry_id} rootEntryId={profile.root_entry_id}
                readDirectoryPage={readDirectoryPage} createDirectory={submitDirectoryCreation} createUploadTask={submitUploadTask}
                sendUploadContent={submitUploadContent} readUploadTask={readUploadTask} readPublishedEntry={readPublishedEntry}
                cancelUploadTask={cancelUploadTask} refreshProfile={refreshProfile}
                authorizeFileAccess={authorizeFileAccess}
                renameEntry={submitEntryRename} moveEntry={submitEntryMove} trashEntry={submitEntryTrash} />
            </div>
            {view === 'trash' && <TrashBrowser rootEntryId={profile.root_entry_id} readTrashPage={readTrashPage}
              readDirectoryPage={readDirectoryPage} readPreview={readPreview} confirmPurge={submitPurgeConfirmation} readProgress={readProgress}
              restoreEntry={submitEntryRestore} />}
          </>
        ) : (
          <form className="auth-form" onSubmit={submitLogin}>
            <label htmlFor="auth-username">用户名</label>
            <Input id="auth-username" name="username" autoComplete="username" required value={username}
              onChange={event => setUsername(event.target.value)} disabled={busy !== null} />
            <label htmlFor="auth-password">密码</label>
            <Input.Password id="auth-password" name="password" autoComplete="current-password" required value={password}
              onChange={event => setPassword(event.target.value)} disabled={busy !== null} />
            <Button type="primary" htmlType="submit" loading={busy === 'login'} disabled={busy !== null}>登录</Button>
          </form>
        ))}
      </section>
    </main>
  )
}
