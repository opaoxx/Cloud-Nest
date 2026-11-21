async page => {
  const baseURL = new URL(page.url()).origin
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(baseURL)) throw new Error('验收只允许访问隔离回环服务')
  page.setDefaultTimeout(12000)
  const uploadsPath = '/api/v1/uploads'
  const posts = []
  const unexpected = []
  let nextCreate = ''
  let originalReceipt
  const assert = (condition, message) => { if (!condition) throw new Error(message) }

  // 1. 只允许隔离登录、读取和逐次授权的创建请求，记录其请求字段。
  await page.context().route('**/*', async route => {
    const request = route.request()
    const { origin, pathname: path } = new URL(request.url())
    if (origin !== baseURL) return route.abort()
    const method = request.method()
    if (method === 'POST' && path === uploadsPath) {
      const headers = request.headers()
      posts.push({
        body: request.postDataJSON(), key: headers['idempotency-key'],
        authorized: (headers.authorization ?? '').startsWith('Bearer '),
        type: headers['content-type'],
      })
      const delivery = nextCreate
      nextCreate = ''
      if (delivery === 'normal') return route.continue()
      if (delivery === 'drop' || delivery === 'bad-path') {
        const created = await route.fetch({ maxRedirects: 0, timeout: 5000 })
        assert(created.status() === 201, '隔离后端应实际交付受理回执')
        const envelope = await created.json()
        if (delivery === 'drop') {
          originalReceipt = envelope.data
          return route.abort('failed')
        }
        assert(envelope.data.task.id === originalReceipt.task.id, '同键重试应返回同一任务')
        envelope.data.content_path = 'https://upload.invalid/other-task/content'
        return route.fulfill({ status: 201, json: envelope })
      }
    } else if ((method === 'GET' && !path.startsWith(uploadsPath)) ||
               (method === 'POST' && /^\/api\/v1\/auth\/(login|refresh)$/.test(path))) {
      return route.continue()
    }
    unexpected.push(`${method} ${path}`)
    return route.abort()
  })

  await page.getByLabel('用户名', { exact: true }).fill('fixture_web_login')
  await page.getByLabel('密码', { exact: true }).fill('fixture-web-login-secret')
  await page.getByRole('button', { name: /^登\s*录$/ }).click()
  await page.getByRole('button', { name: '隔离任务目标目录', exact: true }).click()

  const dialog = page.getByRole('dialog', { name: '准备单文件上传' })
  const openDialog = () => page.getByRole('button', { name: '选择上传文件', exact: true }).click()
  const picker = dialog.getByLabel('选择本地文件', { exact: true })
  const create = dialog.getByRole('button', { name: '创建上传任务', exact: true })
  const chooseFile = (name, size) => picker.evaluate((input, fixture) => {
    const files = new DataTransfer()
    files.items.add(new File([new Uint8Array(fixture.size)], fixture.name))
    input.files = files.files
    input.dispatchEvent(new Event('change', { bubbles: true }))
  }, { name, size })
  const waitForCreate = () => page.waitForResponse(response => new URL(response.url()).pathname === uploadsPath &&
    response.request().method() === 'POST')

  // 2. 选文件时只准备声明。显式连点只发送一次，丢失回执后等待手动重试。
  await openDialog()
  await dialog.getByText('根目录 / 隔离任务目标目录', { exact: true }).waitFor()
  assert(await create.isDisabled(), '没有文件时不可创建任务')
  await chooseFile('COM1.txt', 5)
  await dialog.getByRole('alert').waitFor()
  assert(await create.isDisabled(), '名称无效时不可创建任务')
  const declaration = { parent_id: '54', name: '  任务资料.MD  ', size_bytes: 5 }
  await chooseFile(declaration.name, declaration.size_bytes)
  await dialog.getByText('文件已就绪，尚未上传。').waitFor()
  assert(posts.length === 0, '选择有效文件不应自动发送创建请求')

  nextCreate = 'drop'
  await create.evaluate(button => { button.click(); button.click() })
  await dialog.getByRole('alert').getByText('暂时无法连接到服务，请重试。').waitFor()
  assert(posts.length === 1 && originalReceipt.task.status === 'UPLOADING', '连点及丢失回执只能提交一次')

  nextCreate = 'bad-path'
  await create.click()
  await dialog.getByRole('alert').getByText('服务返回的上传任务回执有误。').waitFor()
  assert(posts.length === 2, '无效回执不应自动重试')

  const accepted = waitForCreate()
  nextCreate = 'normal'
  await create.click()
  const success = await accepted
  assert(success.status() === 201, '手动重试应收到 201')
  const { data } = await success.json()
  assert(data.task.id === originalReceipt.task.id && data.content_path === `${uploadsPath}/${data.task.id}/content`,
    '首次创建和重试应返回同一任务及同源接收路径')
  await dialog.getByText('任务已创建，等待上传文件内容。').waitFor()
  await dialog.locator('dd').getByText(data.task.id, { exact: true }).waitFor()
  assert(await create.count() === 0 && await picker.isDisabled(), '已创建的选择不可重复创建')
  assert(posts.length === 3 && posts.every(request => request.key === posts[0].key &&
    request.authorized && request.type.startsWith('application/json') &&
    Object.keys(request.body).length === 3 && request.body.parent_id === declaration.parent_id &&
    request.body.name === declaration.name && request.body.size_bytes === declaration.size_bytes),
  '同一选择须使用同一键、认证及原始声明')

  // 3. 新的文件选择使用新键，真实后端拒绝超过剩余逻辑配额的创建。
  await dialog.getByRole('button', { name: '关闭', exact: true }).click()
  await dialog.waitFor({ state: 'hidden' })
  await openDialog()
  await chooseFile('配额拒绝.txt', 4)
  const rejected = waitForCreate()
  nextCreate = 'normal'
  await create.click()
  const error = await rejected
  assert(error.status() === 507 && (await error.json()).error.code === 'QUOTA_EXCEEDED', '应收到实际配额错误')
  await dialog.getByRole('alert').getByText('存储空间不足').waitFor()
  const uuid = /^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$/
  assert(posts.length === 4 && uuid.test(posts[0].key) && uuid.test(posts[3].key) && posts[3].key !== posts[0].key &&
    posts[3].body.parent_id === '54' && posts[3].body.size_bytes === 4 && unexpected.length === 0,
  '新文件须使用新键，全程不应发出内容上传或其他业务请求')
  return 'CN523 browser checks passed'
}
