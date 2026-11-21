async page => {
  const baseURL = new URL(page.url()).origin
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(baseURL)) throw new Error('验收只允许访问隔离回环服务')
  page.setDefaultTimeout(12000)
  const textFixture = { id: 65, name: '合成预览.txt' }
  const oversizedFixture = { id: 67, name: '超限预览.txt' }
  const emptyFixture = { id: 68, name: '空预览.txt' }
  const expectedText = '隔离纯文本预览。\n<strong>保持原样</strong> & 符号\n尾行'
  let nextAuthorization = ''
  let nextRead
  let authorizations = 0
  let reads = 0
  let unexpected = 0
  const check = (condition, message) => { if (!condition) throw new Error(message) }

  // 1. 只放行明确发起的授权和精确匹配的隔离正文 GET。
  await page.context().route('**/*', async route => {
    const request = route.request()
    const { origin, pathname } = new URL(request.url())
    const method = request.method()
    if (origin !== baseURL) {
      if (method !== 'GET' || request.url() !== nextRead?.url) {
        unexpected++
        return route.abort()
      }
      const action = nextRead
      nextRead = undefined
      reads++
      const headers = request.headers()
      check(request.resourceType() === 'fetch' && !headers.authorization && !headers.cookie && !headers.referer,
        '只读正文请求不能带网盘凭证或来源地址')
      if (action.reply) {
        return route.fulfill({
          ...action.reply,
          headers: { 'Access-Control-Allow-Origin': baseURL, 'Content-Type': 'text/plain', 'Cache-Control': 'no-store' },
        })
      }
      if (action.gate) await action.gate.promise
      return route.continue()
    }
    if (method === 'GET' || (method === 'POST' && /^\/api\/v1\/auth\/(login|refresh)$/.test(pathname))) {
      return route.continue()
    }
    if (method === 'POST' && pathname === nextAuthorization) {
      nextAuthorization = ''
      check(request.postDataJSON().purpose === 'preview', '授权请求应使用预览用途')
      authorizations++
      return route.continue()
    }
    unexpected++
    return route.abort()
  })

  await page.getByLabel('用户名', { exact: true }).fill('fixture_web_login')
  await page.getByLabel('密码', { exact: true }).fill('fixture-web-login-secret')
  await page.getByRole('button', { name: /^登\s*录$/ }).click()
  const dialog = page.getByRole('dialog', { name: '预览授权' })
  const showText = dialog.getByRole('button', { name: '查看文本', exact: true })
  const refresh = dialog.getByRole('button', { name: '重新获取预览链接', exact: true })
  const content = dialog.locator('pre[aria-label="纯文本正文"]')

  function openPreview(fixture) {
    return page.getByRole('button', { name: '获取预览链接 ' + fixture.name, exact: true })
  }

  async function authorize(button, fixture = textFixture, expectedStatus = 200) {
    const path = '/api/v1/entries/' + fixture.id + '/download'
    const issued = page.waitForResponse(response => new URL(response.url()).pathname === path &&
      response.request().method() === 'POST')
    nextAuthorization = path
    await button.click()
    const response = await issued
    check(response.status() === expectedStatus, '隔离预览授权返回了意外状态')
    const { data, error } = await response.json()
    if (expectedStatus !== 200) return error

    const signed = new URL(data.url)
    check(signed.protocol === 'http:' && signed.hostname === '127.0.0.1' &&
      signed.pathname.endsWith('/preview-fixture/' + fixture.id) && data.mime_type === 'text/plain' &&
      !!signed.searchParams.get('X-Amz-Signature'), '只读取隔离文本的签名地址')
    await showText.waitFor()
    return data
  }

  async function readText(grant, options = {}) {
    nextRead = { url: grant.url, ...options }
    const received = page.waitForResponse(response => response.url() === grant.url && response.request().method() === 'GET')
    await showText.click()
    if (options.gate) {
      await dialog.getByRole('status').getByText('正在加载文本…').waitFor()
      options.gate.resolve()
    }
    return received
  }

  async function closeDialog() {
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    await dialog.waitFor({ state: 'hidden' })
  }

  // 2. 过期时不得读取，续签后使用真实跨源响应验证 UTF-8、换行和标签转义。
  const expired = await authorize(openPreview(textFixture))
  check(reads === 0, '仅授权时不能读取正文')
  await page.clock.setFixedTime(new Date(expired.expires_at))
  await showText.click()
  await dialog.getByRole('alert').getByText('预览链接已过期。').waitFor()
  check(reads === 0 && authorizations === 1, '过期时没有正文请求或自动续签')
  await page.clock.setFixedTime(new Date())
  const loaded = await readText(await authorize(refresh), { gate: Promise.withResolvers() })
  check(loaded.status() === 200, '真实 MinIO 文本需要可读的跨源响应')
  await content.waitFor()
  check(await content.textContent() === expectedText && await content.locator('*').count() === 0,
    '正文须完整显示为普通文本，标签不能生成元素')
  await closeDialog()

  // 3. 注入超出授权的正文和 503。两个错误均可在重新授权后继续查看。
  const mismatched = await readText(await authorize(openPreview(textFixture)), {
    reply: { status: 200, body: 'a'.repeat(2 * 1024 * 1024 + 1) },
  })
  check(mismatched.status() === 200, '应注入一次超过上限的正文')
  await dialog.getByRole('alert').getByText('文本加载失败。文本字节数与授权不一致。').waitFor()
  const failed = await readText(await authorize(refresh), { reply: { status: 503, body: 'fixture' } })
  check(failed.status() === 503, '应注入一次存储读取失败')
  await dialog.getByRole('alert').getByText('文本加载失败。存储未能返回文本正文。').waitFor()
  check(await content.count() === 0 && reads === 3 && authorizations === 4, '失败时不展示正文或自动重试')
  check((await readText(await authorize(refresh))).status() === 200, '手动重新授权应恢复')
  await content.waitFor()
  check(await content.textContent() === expectedText, '恢复后正文仍应一致')
  await closeDialog()

  const empty = await authorize(openPreview(emptyFixture), emptyFixture)
  check((await readText(empty)).status() === 200, '零字节文本应由真实存储返回')
  await dialog.getByRole('status').getByText('此文件为空。').waitFor()
  await closeDialog()

  const rejected = await authorize(openPreview(oversizedFixture), oversizedFixture, 422)
  check(rejected.code === 'UNSUPPORTED_PREVIEW', '实际超过 2 MiB 的文件应被服务端拒绝')
  await dialog.getByRole('button', { name: '改为获取下载链接', exact: true }).waitFor()
  check(await showText.count() === 0 && reads === 5 && authorizations === 7 && unexpected === 0,
    '超限文件没有正文请求，其他请求须全部来自明确点击')
  return 'CN531 browser checks passed'
}
