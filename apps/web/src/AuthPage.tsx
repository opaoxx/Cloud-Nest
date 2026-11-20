import { useCallback, useEffect, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { Button, Input } from 'antd'
import { getCurrentUser, login, refreshWebSession } from './api/auth'
import type { CurrentUserProfile } from './api/auth'
import { readDirectoryPageWithRefresh } from './api/drive'
import type { DirectoryPageReader } from './api/drive'
import { ApiError, apiErrorMessage } from './api/http'
import DirectoryBrowser from './DirectoryBrowser'
import { formatSize } from './demo'
import './auth.css'

export default function AuthPage() {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const activeSession = useRef<{ accessToken: string } | null>(null)
  const [profile, setProfile] = useState<CurrentUserProfile | null>(null)
  const [restoring, setRestoring] = useState(true)
  const [busy, setBusy] = useState(false)
  const [errorMessage, setErrorMessage] = useState('')

  const clearSession = useCallback((error: unknown) => {
    activeSession.current = null
    setProfile(null)
    setErrorMessage(apiErrorMessage(error))
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
      try {
        const renewedToken = await refreshWebSession()
        if (activeSession.current !== session) throw new ApiError(401, '请登录后重试。')
        session.accessToken = renewedToken
        return renewedToken
      } catch (error) {
        if (activeSession.current === session) clearSession(error)
        throw error
      }
    }

    try {
      return await readDirectoryPageWithRefresh(requestedToken, parentId, cursor, signal, renewAccessToken)
    } catch (error) {
      // 3. 列表返回其他 401 或重试仍返回 401 时，清理本地会话。
      if (!signal.aborted && activeSession.current === session && error instanceof ApiError && error.status === 401) {
        clearSession(error)
      }
      throw error
    }
  }, [clearSession])

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
    setBusy(true)
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
      setBusy(false)
    }
  }

  async function readProfileAgain() {
    // 1. 用当前内存会话重新读取资料。
    const session = activeSession.current
    if (!session || busy) return
    const requestedToken = session.accessToken
    setBusy(true)
    setErrorMessage('')
    try {
      const profile = await getCurrentUser(requestedToken)
      if (activeSession.current === session) setProfile(profile)
    } catch (error) {
      // 2. 401 表明需要重新登录，其他失败保留已显示资料。
      if (activeSession.current !== session || session.accessToken !== requestedToken) return
      if (error instanceof ApiError && error.status === 401) clearSession(error)
      else setErrorMessage(apiErrorMessage(error))
    } finally {
      setBusy(false)
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
            <Button loading={busy} onClick={() => void readProfileAgain()}>重新读取资料</Button>
            <DirectoryBrowser key={profile.root_entry_id} rootEntryId={profile.root_entry_id}
              readDirectoryPage={readDirectoryPage} />
          </>
        ) : (
          <form className="auth-form" onSubmit={submitLogin}>
            <label htmlFor="auth-username">用户名</label>
            <Input id="auth-username" name="username" autoComplete="username" required value={username}
              onChange={event => setUsername(event.target.value)} disabled={busy} />
            <label htmlFor="auth-password">密码</label>
            <Input.Password id="auth-password" name="password" autoComplete="current-password" required value={password}
              onChange={event => setPassword(event.target.value)} disabled={busy} />
            <Button type="primary" htmlType="submit" loading={busy} disabled={busy}>登录</Button>
          </form>
        ))}
      </section>
    </main>
  )
}
