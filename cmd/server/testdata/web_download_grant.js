async page => {
  const baseURL = new URL(page.url()).origin
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(baseURL)) throw new Error('验收只允许访问隔离回环服务')
  page.setDefaultTimeout(12000)
  const entriesPath = '/api/v1/entries'
  const entryId = '9007199254740993'
  const currentRevision = '9007199254740995'
  const staleRevision = '9007199254740994'
  const downloadPath = `${entriesPath}/${entryId}/download`
  const missingPath = `${entriesPath}/52/download`
  const grants = []
  const unexpected = []
  let rootReads = 0
  let outsideRequests = 0
  let nextGrant
  const check = (condition, message) => { if (!condition) throw new Error(message) }

  // 1. 模拟列表仍持有旧版本。授权请求转发给隔离后端，禁止页面访问存储地址。
  await page.context().route('**/*', async route => {
    const request = route.request()
    const url = new URL(request.url())
    if (url.origin !== baseURL) {
      outsideRequests++
      return route.abort()
    }
    const method = request.method()
    if (method === 'GET') {
      if (url.pathname === entriesPath && url.searchParams.get('parent_id') === '33') {
        rootReads++
        if (rootReads === 1) {
          const result = await route.fetch({ maxRedirects: 0, timeout: 5000 })
          check(result.status() === 200, '应能读取隔离根目录')
          const body = await result.json()
          const item = body.data.items.find(item => item.id === entryId)
          check(item?.revision === currentRevision, '隔离列表应包含原始完整版本')
          item.revision = staleRevision
          return route.fulfill({ status: 200, json: body })
        }
      }
      return route.continue()
    }
    if (method === 'POST' && /^\/api\/v1\/auth\/(login|refresh)$/.test(url.pathname)) return route.continue()
    if (method === 'POST' && url.pathname === nextGrant?.path) {
      const action = nextGrant
      nextGrant = undefined
      const headers = request.headers()
      grants.push({ path: url.pathname, body: request.postDataJSON(),
        authorized: (headers.authorization ?? '').startsWith('Bearer ') })
      if (action.badUrl) {
        const result = await route.fetch({ maxRedirects: 0, timeout: 5000 })
        check(result.status() === 200, '真实端点应返回下载授权')
        const body = await result.json()
        body.data.url = 'javascript:void(0)'
        return route.fulfill({ status: 200, json: body })
      }
      return route.continue()
    }
    unexpected.push(`${method} ${url.pathname}`)
    return route.abort()
  })

  const responseFor = (method, path, matches = () => true) => page.waitForResponse(response => {
    const url = new URL(response.url())
    return url.origin === baseURL && url.pathname === path && response.request().method() === method && matches(url)
  })

  await page.getByLabel('用户名', { exact: true }).fill('fixture_web_login')
  await page.getByLabel('密码', { exact: true }).fill('fixture-web-login-secret')
  await page.getByRole('button', { name: /^登\s*录$/ }).click()
  await page.getByRole('button', { name: '隔离下载目录', exact: true }).waitFor()
  check(await page.getByRole('button', { name: '获取下载链接 隔离下载目录', exact: true }).count() === 0,
    '目录应继续只提供目录操作')

  const dialog = page.getByRole('dialog', { name: '下载授权' })
  const open = name => page.getByRole('button', { name: `获取下载链接 ${name}`, exact: true })
  const urlField = dialog.getByLabel('短期下载链接', { exact: true })
  async function closeDialog() {
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    await dialog.waitFor({ state: 'hidden' })
  }

  // 2. 旧版本连点一次授权失败，刷新目录后等待用户重新发起。
  const conflict = responseFor('POST', downloadPath)
  const reloaded = responseFor('GET', entriesPath, url => url.searchParams.get('parent_id') === '33' && !url.searchParams.has('cursor'))
  nextGrant = { path: downloadPath }
  await open('隔离授权.bin').evaluate(button => { button.click(); button.click() })
  const rejected = await conflict
  check(rejected.status() === 409 && (await rejected.json()).error.code === 'REVISION_CONFLICT', '旧版本须被真实后端拒绝')
  await dialog.getByRole('alert').getByText('文件版本已变化。请关闭窗口，从更新后的列表重新获取链接。').waitFor()
  check((await reloaded).status() === 200 && grants.length === 1 && await urlField.count() === 0,
    '冲突仅刷新目录，不自动重发授权')
  await closeDialog()

  // 3. 使用新版本，拒绝异常链接后重试并展示真实的短期签名地址。
  const corrupt = responseFor('POST', downloadPath)
  nextGrant = { path: downloadPath, badUrl: true }
  await open('隔离授权.bin').click()
  check((await corrupt).status() === 200, '应模拟一次格式不合约定的成功回执')
  await dialog.getByRole('alert').getByText('服务返回的下载授权有误。').waitFor()
  check(await urlField.count() === 0, '异常地址不能展示')

  const authorized = responseFor('POST', downloadPath)
  nextGrant = { path: downloadPath }
  await dialog.getByRole('button', { name: '重新获取链接', exact: true }).click()
  const success = await authorized
  const { data } = await success.json()
  const signed = new URL(data.url)
  check(success.status() === 200 && data.revision === currentRevision && data.disposition.startsWith('attachment;') &&
    signed.protocol === 'http:' && signed.hostname === '127.0.0.1' && signed.pathname.endsWith('/download-fixture/source.bin') &&
    !!signed.searchParams.get('versionId') && !!signed.searchParams.get('X-Amz-Signature') &&
    signed.searchParams.get('X-Amz-Expires') === '900' && Date.parse(data.expires_at) > Date.now(), '真实签名应绑定对象版本与 15 分钟期限')
  await urlField.waitFor()
  check(await urlField.evaluate((input, expected) => input.readOnly && input.value === expected, data.url),
    '窗口须原样展示签名地址')
  check(Date.parse(await dialog.locator('time').getAttribute('datetime')) === Date.parse(data.expires_at), '窗口须展示签发有效期')
  await closeDialog()

  // 4. 缺少对象时显示后端原因，并确认浏览器没有发送正文请求。
  const unavailable = responseFor('POST', missingPath)
  nextGrant = { path: missingPath }
  await open('缺少内容.bin').click()
  const failed = await unavailable
  const { error } = await failed.json()
  check(failed.status() === 409 && error.code === 'TARGET_UNAVAILABLE', '应显示真实的对象不可用状态')
  await dialog.getByRole('alert').getByText(error.message, { exact: true }).waitFor()
  check(await urlField.count() === 0, '不可用对象不能交付链接')
  check(grants.length === 4 && grants.map(grant => grant.body.expected_revision).join(',') ===
    [staleRevision, currentRevision, currentRevision, '1'].join(',') &&
    grants.every(grant => grant.authorized && grant.body.purpose === 'download' && Object.keys(grant.body).length === 2) &&
    outsideRequests === 0 && unexpected.length === 0, '浏览器仅应发出明确发起的授权请求')
  return 'CN527 browser checks passed'
}
