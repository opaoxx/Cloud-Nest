async page => {
  const baseURL = new URL(page.url()).origin
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(baseURL)) throw new Error('验收只允许访问隔离回环服务')

  const context = page.context()
  page.setDefaultTimeout(12000)
  const requests = []
  const purgePath = '/api/v1/trash/56'
  const restorePath = '/api/v1/trash/60/restore'
  let allowedWrite = ''
  let expireOnRecheck = null
  const assert = (condition, message) => { if (!condition) throw new Error(message) }
  const writes = () => requests.filter(({ method, path }) => method !== 'GET' && !path.startsWith('/api/v1/auth/'))
  const atPath = (url, path) => new URL(url).origin === baseURL && new URL(url).pathname === path

  // 1. 只允许回环服务、合成登录和逐场景明示的业务写入。
  await context.route('**/*', async route => {
    const request = route.request()
    const { origin, pathname: path } = new URL(request.url())
    if (origin !== baseURL) return route.abort()
    const method = request.method()
    if (path.startsWith('/api/')) requests.push({ method, path })
    if (expireOnRecheck && method === 'GET' && path === `${purgePath}/purge-preview`) {
      const expiry = expireOnRecheck
      expireOnRecheck = null
      await page.clock.setFixedTime(expiry)
    }
    if (method === 'GET' || (method === 'POST' && /^\/api\/v1\/auth\/(login|refresh)$/.test(path))) return route.continue()
    if (`${method} ${path}` === allowedWrite) {
      allowedWrite = ''
      return route.continue()
    }
    return route.abort()
  })

  const openTrash = tab => tab.getByRole('button', { name: '回收站', exact: true }).click()
  const popup = page.getByRole('dialog')
  const cancelButton = () => popup.getByRole('button', { name: /^取\s*消$/ })
  async function openPreview(entryName, id) {
    const response = page.waitForResponse(result => atPath(result.url(), `/api/v1/trash/${id}/purge-preview`))
    await page.getByRole('button', { name: `清理预检 ${entryName}`, exact: true }).click()
    const received = await response
    assert(received.status() === 200, '预检应返回 200')
    await popup.getByRole('button', { name: '继续确认', exact: true }).waitFor()
    return (await received.json()).data
  }
  async function enterConfirmation() {
    const displayed = await popup.locator('dl').innerText()
    await popup.getByRole('button', { name: '继续确认', exact: true }).click()
    await page.getByRole('dialog', { name: '确认永久清理' }).waitFor()
    assert(await popup.locator('dl').innerText() === displayed, '确认须展示同一条目和范围')
    assert(await cancelButton().evaluate(button => button === document.activeElement),
      '确认窗口须默认聚焦取消')
  }

  await page.getByLabel('用户名', { exact: true }).fill('fixture_web_login')
  await page.getByLabel('密码', { exact: true }).fill('fixture-web-login-secret')
  await page.getByRole('button', { name: /^登\s*录$/ }).click()
  await openTrash(page)

  // 2. 三种退出方式与过期确认均不得产生任何业务写入。
  for (const close of [
    () => cancelButton().click(),
    () => page.keyboard.press('Escape'),
    () => page.keyboard.press('Enter'),
  ]) {
    const preview = await openPreview('预检回收目录', 56)
    assert(preview.file_count === 2 && preview.directory_count === 2 && preview.logical_bytes === '4096', '隔离预检范围有误')
    const displayed = await popup.locator('dl').innerText()
    assert(displayed.includes('预检回收目录') && displayed.includes('根目录') && /文件数\s+2\s+目录数\s+2/.test(displayed) &&
      displayed.includes('4096 字节') && !displayed.includes(preview.preview_digest), '范围显示有误或泄露预检凭证')
    await enterConfirmation()
    await close()
    await popup.waitFor({ state: 'hidden' })
    assert(writes().length === 0, '取消、Esc 和默认焦点上的 Enter 不能写入')
  }

  const expiring = await openPreview('预检回收目录', 56)
  await enterConfirmation()
  await page.clock.setFixedTime(new Date(expiring.expires_at))
  const beforeExpiry = requests.length
  await popup.getByRole('button', { name: '确认清理', exact: true }).click()
  await popup.getByRole('alert').getByText('清理预检已过期，请重新预检。').waitFor()
  assert(requests.length === beforeExpiry, '到达有效期边界后不能发送复核或写入请求')
  await page.clock.setFixedTime(new Date())
  const freshPreviewResponse = page.waitForResponse(result => atPath(result.url(), `${purgePath}/purge-preview`))
  await popup.getByRole('button', { name: '重新预检', exact: true }).click()
  await popup.getByRole('button', { name: '继续确认', exact: true }).waitFor()
  assert(writes().length === 0, '重新预检不能自动写入')
  const freshPreview = (await (await freshPreviewResponse).json()).data
  await enterConfirmation()
  expireOnRecheck = new Date(freshPreview.expires_at)
  await popup.getByRole('button', { name: '确认清理', exact: true }).click()
  await popup.getByRole('alert').getByText('清理预检已过期，请重新预检。').waitFor()
  assert(writes().length === 0, '复核返回时原凭证已过期，不能继续写入')
  await page.clock.setFixedTime(new Date())
  await cancelButton().click()
  await popup.waitFor({ state: 'hidden' })

  // 3. 第二个标签页恢复条目后，第一个标签页只发出复核 GET 并提示重新预检。
  await openPreview('跨页恢复目录', 60)
  await enterConfirmation()
  const otherTab = await context.newPage()
  await otherTab.goto(baseURL)
  await openTrash(otherTab)
  await otherTab.getByRole('button', { name: '恢复 跨页恢复目录', exact: true }).click()
  const restored = otherTab.waitForResponse(result => atPath(result.url(), restorePath))
  allowedWrite = `POST ${restorePath}`
  await otherTab.getByRole('button', { name: '恢复条目', exact: true }).click()
  assert((await restored).status() === 200, '第二标签页的对照恢复应返回 200')
  await otherTab.close()
  const rechecked = page.waitForResponse(result => atPath(result.url(), '/api/v1/trash/60/purge-preview'))
  await popup.getByRole('button', { name: '确认清理', exact: true }).click()
  assert((await rechecked).status() === 404, '已恢复目标的预检应返回 404')
  await popup.getByRole('alert').getByText('条目或清理范围已变化，请重新预检。').waitFor()
  assert(writes().length === 1 && writes()[0].path === restorePath, '旧预检不能发出 DELETE')
  await popup.getByRole('button', { name: '刷新回收站', exact: true }).click()
  await popup.waitFor({ state: 'hidden' })
  await page.getByRole('button', { name: '清理预检 跨页恢复目录', exact: true }).waitFor({ state: 'hidden' })

  // 4. 仅显式确认发送一次 DELETE，复核中连点也须使用原预检凭证。
  const reviewed = await openPreview('预检回收目录', 56)
  await enterConfirmation()
  const accepted = page.waitForResponse(result => atPath(result.url(), purgePath) && result.request().method() === 'DELETE')
  allowedWrite = `DELETE ${purgePath}`
  await popup.getByRole('button', { name: '确认清理', exact: true }).evaluate(button => {
    button.click()
    button.click()
  })
  const response = await accepted
  assert(response.status() === 202, `确认应返回 202，实际 ${response.status()}`)
  const headers = response.request().headers()
  assert(headers['x-cloudnest-expected-revision'] === reviewed.revision &&
    headers['x-cloudnest-purge-preview'] === reviewed.preview_digest &&
    /^[0-9a-f-]{36}$/.test(headers['idempotency-key'] ?? '') && response.request().postData() === null,
  '请求须准确使用原预检版本、凭证、UUID 幂等键及无请求体')
  await page.getByText('「预检回收目录」的永久清理已受理，正在等待后台处理。').waitFor()
  await page.getByRole('button', { name: '清理预检 预检回收目录', exact: true }).waitFor({ state: 'hidden' })
  assert(writes().length === 2 && writes()[1].method === 'DELETE' && writes()[1].path === purgePath,
    '全程只能发出一次对照恢复和一次清理确认')
  return 'CN520 browser checks passed'
}
