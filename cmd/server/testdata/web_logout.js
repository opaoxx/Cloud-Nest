async page => {
  const baseURL = new URL(page.url()).origin
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(baseURL)) throw new Error('验收只允许访问隔离回环服务')
  page.setDefaultTimeout(12000)
  const [loginPath, refreshPath, logoutPath] = ['login', 'refresh', 'logout'].map(action => `/api/v1/auth/${action}`)
  const profilePath = '/api/v1/me'
  const cookieName = 'cloudnest_refresh_token'
  const authHeaders = { 'X-CloudNest-Client': 'web', Origin: baseURL }
  const check = (condition, message) => { if (!condition) throw new Error(message) }
  const logoutRequests = []
  const unexpected = []
  let failLogout = true
  let expireDirectoryRead = false
  let nextRefresh

  // 1. 使用真实登录和退出。一次模拟失败用于验收保留会话，另一次模拟目录读取遇到过期。
  await page.context().route('**/*', async route => {
    const request = route.request()
    const { origin, pathname: path, searchParams } = new URL(request.url())
    if (origin !== baseURL) return route.abort()
    const method = request.method()
    if (method === 'GET') {
      if (expireDirectoryRead && path === '/api/v1/entries' && searchParams.get('parent_id') === '71') {
        expireDirectoryRead = false
        return route.fulfill({ status: 401, json: { error: { code: 'TOKEN_EXPIRED', message: '访问令牌已过期' } } })
      }
      return route.continue()
    }
    if (method === 'POST' && path === loginPath) return route.continue()
    if (method === 'POST' && path === refreshPath) {
      if (nextRefresh) {
        const { arrived, release } = nextRefresh
        nextRefresh = undefined
        arrived.resolve()
        await release.promise
      }
      return route.continue()
    }
    if (method === 'POST' && path === logoutPath) {
      const headers = request.headers()
      logoutRequests.push({ sameOrigin: headers.origin === baseURL, client: headers['x-cloudnest-client'],
        authorization: headers.authorization, body: request.postDataJSON() })
      if (failLogout) {
        failLogout = false
        return route.fulfill({ status: 503, json: { error: { message: '退出服务暂不可用。' } } })
      }
      return route.continue()
    }
    unexpected.push(`${method} ${path}`)
    return route.abort()
  })

  const responseFor = path => page.waitForResponse(response =>
    new URL(response.url()).pathname === path && response.request().method() === 'POST')
  const username = page.getByLabel('用户名', { exact: true })
  const profile = page.getByRole('heading', { name: '本人资料', exact: true })
  const logout = page.getByRole('button', { name: '退出登录', exact: true })
  const cookies = () => page.context().cookies(`${baseURL}/api/v1/auth/refresh`)
  const firstLogin = responseFor(loginPath)
  await username.fill('fixture_web_login')
  await page.getByLabel('密码', { exact: true }).fill('fixture-web-login-secret')
  await page.getByRole('button', { name: /^登\s*录$/ }).click()
  const loginResponse = await firstLogin
  const { data: first } = await loginResponse.json()
  const loginBody = loginResponse.request().postDataJSON()
  await profile.waitFor()
  check((await cookies()).some(cookie => cookie.name === cookieName && cookie.httpOnly), '真实 Web 登录应交付 HttpOnly Cookie')

  const rejected = responseFor(logoutPath)
  await logout.click()
  check((await rejected).status() === 503, '首次退出应收到模拟 503')
  await page.getByRole('alert').getByText('无法确认退出。退出服务暂不可用。').waitFor()
  check(await profile.isVisible() && await logout.isEnabled() && (await cookies()).some(cookie => cookie.name === cookieName),
    '失败时应保留可手动重试的当前会话')

  // 2. 在独立浏览器上下文建立同一安装标识的对照会话，仅当前页面执行退出。
  const otherContext = await page.context().browser().newContext()
  try {
    const secondLogin = await otherContext.request.post(`${baseURL}${loginPath}`, {
      headers: authHeaders, data: loginBody, maxRedirects: 0,
    })
    const { data: second } = await secondLogin.json()
    check(secondLogin.status() === 200 && second.device.id === first.device.id && second.session.id !== first.session.id,
      '对照登录应在同设备创建独立会话')

    // 3. 让目录请求发起真实刷新并暂停，再点击退出。放行后应先收到刷新，再收到一次退出。
    const arrived = Promise.withResolvers()
    const release = Promise.withResolvers()
    const rotated = responseFor(refreshPath)
    nextRefresh = { arrived, release }
    expireDirectoryRead = true
    await page.getByRole('button', { name: '隔离退出目录', exact: true }).click()
    await Promise.race([arrived.promise, rotated.then(() => { throw new Error('预期在途刷新尚未交付') })])
    const signedOut = responseFor(logoutPath)
    try {
      await logout.evaluate(button => { button.click(); button.click() })
      await page.getByRole('button', { name: '正在退出…', exact: true }).waitFor()
      check(logoutRequests.length === 1, '应等待在途刷新，不提前发出退出')
    } finally {
      release.resolve()
    }
    const refreshResponse = await rotated
    const { data: renewed } = await refreshResponse.json()
    const rotatedCookie = (await refreshResponse.headerValue('set-cookie'))?.split(';', 1)[0]
    check(refreshResponse.status() === 200 && rotatedCookie?.startsWith(`${cookieName}=`), '在途刷新应真实交付新 Cookie')
    const logoutResponse = await signedOut
    const { data: receipt } = await logoutResponse.json()
    const clearedCookie = await logoutResponse.headerValue('set-cookie')
    check(logoutResponse.status() === 200 && receipt.revoked === true && clearedCookie?.startsWith(`${cookieName}=;`) &&
      clearedCookie.includes('Max-Age=0'), '退出必须确认撤销并清理 Cookie')
    await username.waitFor()
    check(await profile.count() === 0 && await page.getByRole('alert').count() === 0 &&
      !(await cookies()).some(cookie => cookie.name === cookieName), '退出后应正常返回登录页，且浏览器不再保留刷新 Cookie')
    check(logoutRequests.length === 2 && logoutRequests.every(request => request.sameOrigin && request.client === 'web' &&
      request.authorization === `Bearer ${first.access_token}` && JSON.stringify(request.body) === JSON.stringify({ client_type: 'web' })),
    '两次明确点击各发一次退出，沿用同一会话凭证和既有请求体')

    // 4. 核对服务端拒绝已撤销的凭证、保留对照会话，页面重载仍停在登录页。
    const readProfile = (request, token) => request.get(`${baseURL}${profilePath}`, {
      headers: { Authorization: `Bearer ${token}` }, maxRedirects: 0,
    })
    const revoked = await readProfile(page.request, renewed.access_token)
    check(revoked.status() === 401 && (await revoked.json()).error.code === 'SESSION_REVOKED', '撤销后旧会话的访问令牌应被拒绝')
    const control = await readProfile(otherContext.request, second.access_token)
    check(control.status() === 200 && (await control.json()).data.user.id === first.user.id, '同设备对照会话仍可读取本人资料')
    const replay = await page.request.post(`${baseURL}${refreshPath}`, {
      headers: { ...authHeaders, Cookie: rotatedCookie }, data: { client_type: 'web' }, maxRedirects: 0,
    })
    check(replay.status() === 401, '已撤销会话的刷新 Cookie 应被拒绝')
    const restore = responseFor(refreshPath)
    await page.reload()
    check((await restore).status() === 401, '退出后重载不能恢复当前会话')
    await username.waitFor()
    check(await profile.count() === 0 && unexpected.length === 0, '重载应保留登录页，且没有文件写请求')
  } finally {
    await otherContext.close()
  }
  return 'CN534 browser checks passed'
}
