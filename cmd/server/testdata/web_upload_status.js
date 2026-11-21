async page => {
  const baseURL = new URL(page.url()).origin
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(baseURL)) throw new Error('验收只允许访问隔离回环服务')
  page.setDefaultTimeout(12000)
  const uploadPath = '/api/v1/uploads'
  const fixtures = new Map([
    ['超时复核.bin', { id: '73', size: 3 }],
    ['待校验.bin', { id: '74', size: 6 }],
    ['完成示例.bin', { id: '9007199254740993', size: 8 }],
    ['失败示例.bin', { id: '76', size: 5 }],
    ['其他账号任务.bin', { id: '77', size: 2 }],
  ])
  const checks = []
  const unexpected = []
  let nextQuery
  let nextCreate
  let nextContent
  const assert = (condition, message) => { if (!condition) throw new Error(message) }

  // 1. 仅登录、读取与预定的模拟回执可通过。任务查询直接转发给隔离后端。
  await page.context().route('**/*', async route => {
    const request = route.request()
    const { origin, pathname: path } = new URL(request.url())
    if (origin !== baseURL) return route.abort()
    const method = request.method()
    if ((method === 'GET' && !path.startsWith(uploadPath)) ||
        (method === 'POST' && /^\/api\/v1\/auth\/(login|refresh)$/.test(path))) return route.continue()
    if (method === 'POST' && path === uploadPath && nextCreate) {
      const name = nextCreate
      nextCreate = undefined
      const fixture = fixtures.get(name)
      const body = request.postDataJSON()
      assert(body.parent_id === '33' && body.name === name && body.size_bytes === fixture.size, '预置任务与声明不一致')
      return route.fulfill({ status: 201, json: { data: {
        task: { id: fixture.id, parent_id: '33', size_bytes: fixture.size, status: 'UPLOADING', upload_mode: 'SINGLE' },
        content_path: `${uploadPath}/${fixture.id}/content`, upload_mode: 'SINGLE',
      } } })
    }
    if (method === 'PUT' && path === nextContent?.path) {
      const delivery = nextContent
      nextContent = undefined
      return route.fulfill(delivery.reply)
    }
    if (method === 'GET' && path === nextQuery?.path) {
      const query = nextQuery
      nextQuery = undefined
      const headers = request.headers()
      assert((headers.authorization ?? '').startsWith('Bearer ') && request.postData() === null && !headers['idempotency-key'],
        '查询任务须使用当前会话且只有 GET')
      checks.push(path)
      if (query.reply) return route.fulfill(query.reply)
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
  const queryButton = dialog.getByRole('button', { name: '查询任务状态', exact: true })
  const status = dialog.getByRole('status')
  async function openTask(name) {
    const fixture = fixtures.get(name)
    await page.getByRole('button', { name: '选择上传文件', exact: true }).click()
    await picker.evaluate((input, { name, size }) => {
      const files = new DataTransfer()
      files.items.add(new File([new Uint8Array(size)], name))
      input.files = files.files
      input.dispatchEvent(new Event('change', { bubbles: true }))
    }, { name, size: fixture.size })
    nextCreate = name
    await dialog.getByRole('button', { name: '创建上传任务', exact: true }).click()
    await dialog.getByText('任务已创建，等待上传文件内容。').waitFor()
    return fixture.id
  }
  async function query(taskId, reply) {
    const path = `${uploadPath}/${taskId}`
    const response = page.waitForResponse(result => new URL(result.url()).pathname === path && result.request().method() === 'GET')
    nextQuery = { path, reply }
    await queryButton.click()
    return await response
  }
  async function closeTask() {
    await dialog.getByRole('button', { name: '关闭', exact: true }).click()
    await dialog.waitFor({ state: 'hidden' })
  }

  // 2. 核对三类后端状态、发布结果及短暂读取失败后的手动复核。
  const verifyingId = await openTask('待校验.bin')
  assert(checks.length === 0, '打开任务时不自动查询')
  assert((await query(verifyingId)).status() === 200, '校验中任务应可查询')
  await status.getByText('校验中', { exact: true }).waitFor()
  await query(verifyingId, { status: 200, json: { data: {
    id: '999', upload_mode: 'SINGLE', status: 'VERIFYING', result_entry_id: null, resolved_name: null, last_error: null,
  } } })
  await dialog.getByRole('alert').getByText('无法查询上传任务。服务返回的上传任务状态有误。').waitFor()
  await status.getByText('校验中', { exact: true }).waitFor()
  assert((await query(verifyingId)).status() === 200, '失败后可手动重查')
  await dialog.getByRole('alert').waitFor({ state: 'hidden' })
  assert(await send.count() === 0, '校验中的任务不应再发送正文')
  await closeTask()

  const completedId = await openTask('完成示例.bin')
  nextContent = { path: `${uploadPath}/${completedId}/content`, reply: {
    status: 202, json: { data: { task_id: completedId, status: 'VERIFYING' } },
  } }
  await send.click()
  await status.getByText('文件内容已接收，校验中。').waitFor()
  assert((await query(completedId)).status() === 200, '完成状态来自真实查询')
  await status.getByText('已完成', { exact: true }).waitFor()
  await dialog.locator('dd').getByText('完成示例 (1).bin', { exact: true }).waitFor()
  await dialog.locator('dd').getByText('9007199254740995', { exact: true }).waitFor()
  await closeTask()

  const failedId = await openTask('失败示例.bin')
  assert((await query(failedId)).status() === 200, '失败状态来自真实查询')
  await status.getByText('失败', { exact: true }).waitFor()
  await dialog.getByRole('alert').getByText('文件校验失败，请重新上传', { exact: true }).waitFor()
  await closeTask()

  // 3. 模拟超时后复核同一任务，跨账号目标仍由服务端拒绝。
  const pendingId = await openTask('超时复核.bin')
  nextContent = { path: `${uploadPath}/${pendingId}/content`, reply: {
    status: 408, json: { error: { code: 'UPLOAD_TIMEOUT', message: '上传传输超时，请查询任务状态后重试' } },
  } }
  await send.click()
  await dialog.getByRole('alert').getByText('上传传输超时，请查询任务状态后重试').waitFor()
  assert((await query(pendingId)).status() === 200, '可复核超时后的原任务')
  await status.getByText('待接收或接收中', { exact: true }).waitFor()
  assert(await send.count() === 0, '读到 UPLOADING 也不能直接恢复超时后的发送')
  await closeTask()

  const foreignId = await openTask('其他账号任务.bin')
  const denied = await query(foreignId)
  assert(denied.status() === 404 && (await denied.json()).error.code === 'UPLOAD_NOT_FOUND', '跨账号须返回既有的 404')
  await dialog.getByRole('alert').getByText('无法查询上传任务。上传任务不存在').waitFor()
  assert(checks.length === 7 && unexpected.length === 0, '只能对当前任务发出手动查询')
  return 'CN525 browser checks passed'
}
