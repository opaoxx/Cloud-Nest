async page => {
  const baseURL = new URL(page.url()).origin
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(baseURL)) throw new Error('验收只允许访问隔离回环服务')
  page.setDefaultTimeout(12000)
  const uploadPath = '/api/v1/uploads'
  const contentRequests = []
  const unexpected = []
  let allowCreate = false
  let nextContent
  const check = (condition, message) => { if (!condition) throw new Error(message) }

  // 1. 只在按下对应按钮后放行 POST 或 PUT。模拟超时只发生在浏览器与隔离服务之间。
  await page.context().route('**/*', async route => {
    const request = route.request()
    const { origin, pathname: path } = new URL(request.url())
    if (origin !== baseURL) return route.abort()
    const method = request.method()
    if ((method === 'GET' && !path.startsWith(uploadPath)) ||
        (method === 'POST' && /^\/api\/v1\/auth\/(login|refresh)$/.test(path))) return route.continue()
    if (method === 'POST' && path === uploadPath && allowCreate) {
      allowCreate = false
      return route.continue()
    }
    if (method === 'PUT' && path === nextContent?.path) {
      const delivery = nextContent
      nextContent = undefined
      const headers = request.headers()
      const body = request.postDataBuffer() ?? []
      contentRequests.push({ path, body: [...body], type: headers['content-type'],
        authorized: (headers.authorization ?? '').startsWith('Bearer ') })
      if (delivery.timeout) {
        return route.fulfill({ status: 408, json: { error: {
          code: 'UPLOAD_TIMEOUT', message: '上传传输超时，请查询任务状态后重试',
        } } })
      }
      if (delivery.gate) await delivery.gate.promise
      return route.continue()
    }
    unexpected.push(`${method} ${path}`)
    return route.abort()
  })

  await page.getByLabel('用户名', { exact: true }).fill('fixture_web_login')
  await page.getByLabel('密码', { exact: true }).fill('fixture-web-login-secret')
  await page.getByRole('button', { name: /^登\s*录$/ }).click()

  const dialog = page.getByRole('dialog', { name: '准备单文件上传' })
  const picker = dialog.getByLabel('选择本地文件', { exact: true })
  const send = dialog.getByRole('button', { name: '上传文件内容', exact: true })
  const verifying = dialog.getByRole('status').getByText('文件内容已接收，校验中。')

  async function createTask(name, bytes) {
    await page.getByRole('button', { name: '选择上传文件', exact: true }).click()
    await picker.evaluate((input, fixture) => {
      const files = new DataTransfer()
      files.items.add(new File([new Uint8Array(fixture.bytes)], fixture.name, { type: 'text/plain' }))
      input.files = files.files
      input.dispatchEvent(new Event('change', { bubbles: true }))
    }, { name, bytes })
    const received = page.waitForResponse(response => new URL(response.url()).pathname === uploadPath &&
      response.request().method() === 'POST')
    allowCreate = true
    await dialog.getByRole('button', { name: '创建上传任务', exact: true }).click()
    const response = await received
    check(response.status() === 201, '选择的文件应通过真实后端创建任务')
    const { data } = await response.json()
    await dialog.getByText('任务已创建，等待上传文件内容。').waitFor()
    return { id: data.task.id, path: data.content_path }
  }

  function waitForContent(path) {
    return page.waitForResponse(response => new URL(response.url()).pathname === path && response.request().method() === 'PUT')
  }

  // 2. 两次真实发送分别验收二进制文件和零字节文件。
  for (const [name, bytes] of [['隔离二进制.bin', [0, 255, 17, 128, 10, 42]], ['隔离空文件.bin', []]]) {
    const before = contentRequests.length
    const task = await createTask(name, bytes)
    check(contentRequests.length === before, '仅创建任务时不得发送文件')
    const accepted = waitForContent(task.path)
    const gate = Promise.withResolvers()
    nextContent = { path: task.path, gate }
    await send.evaluate(button => { button.click(); button.click() })
    await dialog.getByRole('status').getByText('正在发送文件内容…').waitFor()
    gate.resolve()
    const response = await accepted
    const { data } = await response.json()
    check(response.status() === 202 && data.task_id === task.id && data.status === 'VERIFYING', '应收到当前任务的 202')
    await verifying.waitFor()
    const recorded = contentRequests.at(-1)
    check(contentRequests.length === before + 1 && recorded.path === task.path && recorded.type === 'application/octet-stream' &&
      recorded.authorized && JSON.stringify(recorded.body) === JSON.stringify(bytes), '每个任务只发送一次原始字节')
    check(await send.count() === 0, '校验中的任务不能重复发送正文')
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    await dialog.waitFor({ state: 'hidden' })
  }

  // 3. 超时后保留任务 ID，提示先查状态，当前窗口不再发送此正文。
  const task = await createTask('隔离超时.bin', [1, 2, 3])
  const timedOut = waitForContent(task.path)
  nextContent = { path: task.path, timeout: true }
  await send.click()
  check((await timedOut).status() === 408, '应交付模拟超时结果')
  await dialog.getByRole('alert').getByText('上传传输超时，请查询任务状态后重试').waitFor()
  await dialog.getByText('请保留任务 ID，查询任务状态后再重试。').waitFor()
  await dialog.locator('dd').getByText(task.id, { exact: true }).waitFor()
  check(await send.count() === 0 && await verifying.count() === 0 && contentRequests.length === 3 && unexpected.length === 0,
    '超时不得显示校验中、重传或发出其他上传请求')
  return 'CN524 browser checks passed'
}
