async page => {
  const baseURL = new URL(page.url()).origin
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(baseURL)) throw new Error('验收只允许访问隔离回环服务')
  page.setDefaultTimeout(12000)
  const grantPath = '/api/v1/entries/9007199254740993/download'
  const filename = '隔离授权.bin'
  const storageRequests = []
  const unexpected = []
  let nextGrant = false
  let authorizations = 0
  let allowedDownloadUrl = ''
  const check = (condition, message) => { if (!condition) throw new Error(message) }

  // 1. 只放行隔离服务、显式发起的授权和本轮确认的储存 GET。
  await page.context().route('**/*', route => {
    const request = route.request()
    const url = new URL(request.url())
    const method = request.method()
    if (url.origin !== baseURL) {
      if (!allowedDownloadUrl || request.url() !== allowedDownloadUrl || method !== 'GET') {
        unexpected.push('非预期的正文请求')
        return route.abort()
      }
      const headers = request.headers()
      storageRequests.push({ authorization: headers.authorization, referer: headers.referer })
      return route.continue()
    }
    if (method === 'GET' || (method === 'POST' && /^\/api\/v1\/auth\/(login|refresh)$/.test(url.pathname))) {
      return route.continue()
    }
    if (method === 'POST' && url.pathname === grantPath && nextGrant) {
      nextGrant = false
      authorizations++
      return route.continue()
    }
    unexpected.push('非预期的同源写请求')
    return route.abort()
  })

  await page.getByLabel('用户名', { exact: true }).fill('fixture_web_login')
  await page.getByLabel('密码', { exact: true }).fill('fixture-web-login-secret')
  await page.getByRole('button', { name: /^登\s*录$/ }).click()

  const dialog = page.getByRole('dialog', { name: '下载授权' })
  const download = dialog.getByRole('link', { name: '下载文件', exact: true })
  async function authorize(button) {
    const issued = page.waitForResponse(response => new URL(response.url()).pathname === grantPath &&
      response.request().method() === 'POST')
    nextGrant = true
    await button.click()
    const response = await issued
    check(response.status() === 200, '隔离服务应签发真实授权')
    const { data } = await response.json()
    await download.waitFor()
    check(await download.getAttribute('href') === data.url, '下载控件须原样使用签名链接')
    return data
  }

  // 2. 在点击瞬间模拟授权到期，核对页面拦截储存请求和自动续签。
  const grant = await authorize(page.getByRole('button', { name: '获取下载链接 ' + filename, exact: true }))
  await page.clock.setFixedTime(new Date(grant.expires_at))
  await download.click()
  await dialog.getByRole('alert').getByText('下载链接已过期，请重新获取。').waitFor()
  check(authorizations === 1 && storageRequests.length === 0 && unexpected.length === 0 &&
    page.context().pages().length === 1 && await download.count() === 0, '过期点击不能离开页面或发出请求')

  // 3. 手动重新获取，再等浏览器原生附件下载并保存。
  await page.clock.setFixedTime(new Date())
  const renewed = await authorize(dialog.getByRole('button', { name: '重新获取链接', exact: true }))
  const signed = new URL(renewed.url)
  check(signed.protocol === 'http:' && signed.hostname === '127.0.0.1' && signed.origin !== baseURL &&
    signed.pathname.endsWith('/download-fixture/source.bin') && !!signed.searchParams.get('X-Amz-Signature') &&
    renewed.disposition.startsWith('attachment;'), '只允许访问本轮隔离对象的附件地址')
  allowedDownloadUrl = renewed.url
  const saved = page.waitForEvent('download')
  await download.click()
  const artifact = await saved
  check(artifact.url() === renewed.url && artifact.suggestedFilename() === filename, '浏览器须使用服务器给出的中文附件名')
  await artifact.saveAs(filename)
  check(await artifact.failure() === null && new URL(page.url()).origin === baseURL && await dialog.isVisible(),
    '下载完成后应保留当前授权窗口')
  check(authorizations === 2 && storageRequests.length === 1 && !storageRequests[0].authorization &&
    !storageRequests[0].referer && unexpected.length === 0, '只应发送一次无 Bearer 和来源头的签名 GET')
  return 'CN528 browser checks passed'
}
