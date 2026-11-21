async page => {
  const baseURL = new URL(page.url()).origin
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(baseURL)) throw new Error('验收只允许访问隔离回环服务')
  page.setDefaultTimeout(12000)
  const uploads = '/api/v1/uploads'
  const entries = '/api/v1/entries'
  const reads = []
  const unexpected = []
  let allowedWrite = ''
  let currentTaskPath = ''
  let authorization = ''
  const check = (condition, message) => { if (!condition) throw new Error(message) }

  // 1. 只允许本轮真实创建、正文上传及读取。第一次详情请求模拟暂时失败。
  await page.context().route('**/*', route => {
    const request = route.request()
    const url = new URL(request.url())
    if (url.origin !== baseURL) return route.abort()
    const method = request.method()
    const path = url.pathname
    if (method === 'GET') {
      if (path.startsWith(uploads) && path !== currentTaskPath) {
        unexpected.push(`${method} ${path}`)
        return route.abort()
      }
      if (path.startsWith('/api/')) reads.push({ path, parentId: url.searchParams.get('parent_id'), cursor: url.searchParams.get('cursor') })
      if (path.startsWith(`${entries}/`) && reads.filter(read => read.path.startsWith(`${entries}/`)).length === 1) {
        return route.fulfill({ status: 503, json: { error: { message: '详情服务暂不可用' } } })
      }
      return route.continue()
    }
    if (method === 'POST' && /^\/api\/v1\/auth\/(login|refresh)$/.test(path)) return route.continue()
    if (`${method} ${path}` === allowedWrite) {
      allowedWrite = ''
      if (method === 'PUT') authorization = request.headers().authorization ?? ''
      return route.continue()
    }
    unexpected.push(`${method} ${path}`)
    return route.abort()
  })

  function responseFor(method, path, matches = () => true) {
    return page.waitForResponse(response => {
      const url = new URL(response.url())
      return url.origin === baseURL && url.pathname === path && response.request().method() === method && matches(url)
    }).catch(error => {
      throw new Error(`等待 ${method} ${path} 失败。${error.message}\n最近读取 ${JSON.stringify(reads.slice(-8))}，意外请求 ${JSON.stringify(unexpected)}`)
    })
  }

  const targetReads = () => reads.filter(read => read.path === entries && read.parentId === '54')
  const detailReads = () => reads.filter(read => read.path.startsWith(`${entries}/`))
  const originalName = '重名资料.bin'
  const publishedName = '重名资料 (1).bin'

  await page.getByLabel('用户名', { exact: true }).fill('fixture_web_login')
  await page.getByLabel('密码', { exact: true }).fill('fixture-web-login-secret')
  await page.getByRole('button', { name: /^登\s*录$/ }).click()
  await page.getByRole('button', { name: '隔离发布目录', exact: true }).click()
  await page.getByRole('button', { name: originalName, exact: true }).waitFor()
  await page.getByRole('button', { name: '选择上传文件', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '准备单文件上传' })
  const status = dialog.getByRole('status')
  await dialog.getByLabel('选择本地文件', { exact: true }).evaluate((input, name) => {
    const files = new DataTransfer()
    files.items.add(new File([new Uint8Array([13, 42, 0, 255])], name))
    input.files = files.files
    input.dispatchEvent(new Event('change', { bubbles: true }))
  }, originalName)

  // 2. 上传实际合成文件。用测试探针等到后台发布，再点击页面的查询按钮。
  const created = responseFor('POST', uploads)
  allowedWrite = `POST ${uploads}`
  await dialog.getByRole('button', { name: '创建上传任务', exact: true }).click()
  const createdResponse = await created
  check(createdResponse.status() === 201, '应创建当前上传任务')
  const { data: receipt } = await createdResponse.json()
  currentTaskPath = `${uploads}/${receipt.task.id}`
  check(receipt.task.parent_id === '54', '任务的目标须是子目录 54')
  const sent = responseFor('PUT', receipt.content_path)
  allowedWrite = `PUT ${receipt.content_path}`
  await dialog.getByRole('button', { name: '上传文件内容', exact: true }).click()
  check((await sent).status() === 202, '正文应收到 202')
  await status.getByText('文件内容已接收，校验中。').waitFor()

  const query = async () => {
    const requested = responseFor('GET', currentTaskPath)
    await dialog.getByRole('button', { name: '查询任务状态', exact: true }).click()
    const response = await requested
    check(response.status() === 200, '任务状态读取应成功')
    return (await response.json()).data
  }
  check(authorization.startsWith('Bearer '), '正文请求应使用隔离会话')
  let completed = false
  const deadline = Date.now() + 15000
  do {
    const probe = await page.request.get(`${baseURL}${currentTaskPath}`, {
      headers: { Authorization: authorization }, maxRedirects: 0, timeout: 2000,
    })
    check(probe.status() === 200, '隔离等待探针应能读取任务')
    const { data } = await probe.json()
    await probe.dispose()
    check(data.status === 'VERIFYING' || data.status === 'COMPLETED', '合成任务应正常推进')
    completed = data.status === 'COMPLETED'
    if (completed) break
    await page.waitForTimeout(250)
  } while (Date.now() < deadline)
  check(completed && detailReads().length === 0 && targetReads().length === 1, '后台发布前后都不可自动刷新页面')
  const result = await query()
  check(result.status === 'COMPLETED' && result.resolved_name === publishedName && !!result.result_entry_id,
    '真实后台应编号发布并返回结果 ID')
  const detailPath = `${entries}/${result.result_entry_id}`

  // 3. 详情失败时保留任务结果。重试读回同一文件后才刷新子目录首页。
  await status.getByText('已完成', { exact: true }).waitFor()
  await dialog.getByRole('alert').getByText('无法读取发布文件。详情服务暂不可用').waitFor()
  check(detailReads().length === 1 && detailReads()[0].path === detailPath && targetReads().length === 1,
    '详情失败时应保留已完成状态并等用户重试')
  const entry = responseFor('GET', detailPath)
  const refreshed = responseFor('GET', entries, url => url.searchParams.get('parent_id') === '54' && !url.searchParams.has('cursor'))
  const retried = await query()
  check(retried.result_entry_id === result.result_entry_id, '重试须读取同一个结果')
  const entryResponse = await entry
  const { data: file } = await entryResponse.json()
  check(entryResponse.status() === 200 && file.id === result.result_entry_id && file.parent_id === '54' && file.name === publishedName,
    '文件详情应指向真实发布项')
  const listingResponse = await refreshed
  const listing = (await listingResponse.json()).data
  check(listingResponse.status() === 200 && listing.items.some(item => item.id === file.id && item.name === file.name),
    '刷新后的目录页应实际包含发布文件')
  await dialog.getByText('当前文件名', { exact: true }).waitFor()
  await dialog.getByRole('alert').waitFor({ state: 'hidden' })
  await dialog.getByRole('button', { name: '关闭', exact: true }).click()
  await page.getByRole('row').filter({ hasText: publishedName }).waitFor()
  await page.getByRole('button', { name: originalName, exact: true }).waitFor()
  check(detailReads().length === 2 && detailReads()[1].path === detailPath && targetReads().length === 2 &&
    reads.indexOf(targetReads()[1]) > reads.indexOf(detailReads()[1]) &&
    reads.filter(read => read.path === currentTaskPath).length === 2 && unexpected.length === 0,
  '只在成功读到详情后刷新原目录首页')
  return 'CN526 browser checks passed'
}
