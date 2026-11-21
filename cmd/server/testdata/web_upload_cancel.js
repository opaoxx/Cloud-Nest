async page => {
  const baseURL = new URL(page.url()).origin
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(baseURL)) throw new Error('验收只允许访问隔离回环服务')
  page.setDefaultTimeout(12000)
  const uploadPath = '/api/v1/uploads'
  const cancelRequests = []
  const unexpected = []
  let nextCreate
  let nextCancel
  const check = (condition, message) => { if (!condition) throw new Error(message) }

  // 1. 主场景由真实 API 创建任务。两个预置对照任务只模拟创建回执，取消直接访问隔离实例。
  await page.context().route('**/*', async route => {
    const request = route.request()
    const { origin, pathname: path } = new URL(request.url())
    if (origin !== baseURL) return route.abort()
    const method = request.method()
    if (method === 'GET' || (method === 'POST' && /^\/api\/v1\/auth\/(login|refresh)$/.test(path))) return route.continue()
    if (method === 'POST' && path === uploadPath && nextCreate) {
      const { fixtureId } = nextCreate
      nextCreate = undefined
      if (!fixtureId) return route.continue()
      return route.fulfill({ status: 201, json: { data: {
        task: { id: fixtureId, parent_id: '33', size_bytes: 3, status: 'UPLOADING', upload_mode: 'SINGLE' },
        upload_mode: 'SINGLE', content_path: `${uploadPath}/${fixtureId}/content`,
      } } })
    }
    if (method === 'POST' && path === nextCancel?.path) {
      const { wrongTask, gate } = nextCancel
      nextCancel = undefined
      const headers = request.headers()
      cancelRequests.push({ path, authorized: (headers.authorization ?? '').startsWith('Bearer '),
        hasIdempotencyKey: 'idempotency-key' in headers, bytes: request.postDataBuffer()?.length ?? 0 })
      if (gate) await gate.promise
      if (wrongTask) {
        return route.fulfill({ status: 202, json: { data: {
          id: '999', upload_mode: 'SINGLE', status: 'CANCELLING', result_entry_id: null, resolved_name: null, last_error: null,
        } } })
      }
      return route.continue()
    }
    unexpected.push(`${method} ${path}`)
    return route.abort()
  })

  await page.getByLabel('用户名', { exact: true }).fill('fixture_web_login')
  await page.getByLabel('密码', { exact: true }).fill('fixture-web-login-secret')
  await page.getByRole('button', { name: /^登\s*录$/ }).click()
  const dialog = page.getByRole('dialog', { name: '准备单文件上传' })
  const button = name => dialog.getByRole('button', { name, exact: true })
  const cancel = button('取消上传任务')
  const stay = button('暂不取消')
  const confirm = button('确认取消上传')
  const send = button('上传文件内容')
  const query = button('查询任务状态')
  const status = label => dialog.getByRole('status').getByText(label, { exact: true })
  const waitForResponse = (path, method) => page.waitForResponse(response =>
    new URL(response.url()).pathname === path && response.request().method() === method)

  async function openTask(name, fixtureId) {
    await page.getByRole('button', { name: '选择上传文件', exact: true }).click()
    await dialog.getByLabel('选择本地文件', { exact: true }).evaluate((input, filename) => {
      const files = new DataTransfer()
      files.items.add(new File([new Uint8Array([1, 2, 3])], filename))
      input.files = files.files
      input.dispatchEvent(new Event('change', { bubbles: true }))
    }, name)
    check(await cancel.count() === 0, '还没有任务 ID 时不得取消')
    const received = waitForResponse(uploadPath, 'POST')
    nextCreate = { fixtureId }
    await button('创建上传任务').click()
    const response = await received
    check(response.status() === 201, '上传窗口应收到 201 创建回执')
    const { data } = await response.json()
    await dialog.getByText('任务已创建，等待上传文件内容。').waitFor()
    return `${uploadPath}/${data.task.id}`
  }

  async function submitCancel(taskPath, { wrongTask, doubleClick } = {}) {
    await cancel.click()
    const path = `${taskPath}/cancel`
    const received = waitForResponse(path, 'POST')
    const gate = doubleClick ? Promise.withResolvers() : null
    nextCancel = { path, wrongTask, gate }
    if (gate) {
      await confirm.evaluate(button => { button.click(); button.click() })
      await button('正在提交取消…').waitFor()
      check(await query.count() === 0 && await send.count() === 0, '提交途中不得并发查询或上传')
      gate.resolve()
    } else {
      await confirm.click()
    }
    return received
  }

  // 2. 暂不取消、默认焦点上的 Enter 和 Esc 均只回到原上传窗口。
  const taskPath = await openTask('隔离取消.bin')
  await cancel.click()
  await stay.click()
  await cancel.click()
  await stay.waitFor()
  check(await stay.evaluate(target => document.activeElement === target), '确认界面应默认聚焦暂不取消')
  await page.keyboard.press('Enter')
  await cancel.waitFor()
  await cancel.click()
  await page.keyboard.press('Escape')
  await cancel.waitFor()
  const initialRead = waitForResponse(taskPath, 'GET')
  await query.click()
  const { data: initial } = await (await initialRead).json()
  await status('待接收或接收中').waitFor()
  check(initial.status === 'UPLOADING' && cancelRequests.length === 0 && unexpected.length === 0, '退出确认不得改变任务或发出写请求')

  // 3. 错任务回执不能视为受理。先查询同一任务，再显式确认，连点也只交付一次请求。
  await submitCancel(taskPath, { wrongTask: true })
  await dialog.getByRole('alert').getByText(/上传任务状态有误.*请先查询任务状态/).waitFor()
  check(await cancel.isDisabled() && await send.isDisabled(), '回执异常后应先只读核对状态')
  const recheck = waitForResponse(taskPath, 'GET')
  await query.click()
  check((await recheck).status() === 200, '核对必须使用同一任务的 GET')
  const accepted = await submitCancel(taskPath, { doubleClick: true })
  const { data: receipt } = await accepted.json()
  check(accepted.status() === 202 && `${uploadPath}/${receipt.id}` === taskPath && receipt.status === 'CANCELLING',
    '真实 202 应返回同任务取消中')
  await status('取消中').waitFor()
  await dialog.getByText('取消已受理，后台正在处理。可查询任务状态确认结果。').waitFor()
  check(await cancel.count() === 0 && await send.count() === 0 && await query.isEnabled(), '受理后只保留查询入口')
  await button('关闭').click()
  await dialog.waitFor({ state: 'hidden' })

  // 4. 核对服务端的租约冲突提示，以及并发情况下已经过期的真实终态。
  const busyTaskPath = await openTask('隔离占用.bin', '81')
  const busy = await submitCancel(busyTaskPath)
  check(busy.status() === 409 && (await busy.json()).error.code === 'UPLOAD_BUSY', '有效租约应得到真实的忙碌冲突')
  await dialog.getByRole('alert').getByText(/上传任务正在处理中.*请先查询任务状态/).waitFor()
  check(await cancel.isDisabled() && await query.isEnabled(), '冲突后页面应等待状态查询')
  await button('关闭').click()
  await dialog.waitFor({ state: 'hidden' })

  const expiredTaskPath = await openTask('隔离过期.bin', '82')
  const terminal = await submitCancel(expiredTaskPath)
  check(terminal.status() === 200 && (await terminal.json()).data.status === 'EXPIRED', '已过期的任务应返回真实 200 终态')
  await status('已过期').waitFor()
  check(await dialog.getByText(/取消已受理/).count() === 0 && await cancel.count() === 0 && await send.count() === 0,
    '已过期应按实际终态显示')

  const expectedPaths = [taskPath, taskPath, busyTaskPath, expiredTaskPath].map(path => `${path}/cancel`)
  check(unexpected.length === 0 && JSON.stringify(cancelRequests.map(request => request.path)) === JSON.stringify(expectedPaths) &&
    cancelRequests.every(request => request.authorized && request.bytes === 0 && !request.hasIdempotencyKey),
  '只允许显式确认的请求，且每次使用同一任务的 Bearer、无正文和无幂等键')
  return 'CN532 browser checks passed'
}
