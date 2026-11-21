async page => {
  const baseURL = new URL(page.url()).origin
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(baseURL)) throw new Error('验收只允许访问隔离回环服务')
  page.setDefaultTimeout(12000)
  const uploads = '/api/v1/uploads'
  const taskPath = `${uploads}/81`
  const cancelPath = `${taskPath}/cancel`
  const profilePath = '/api/v1/me'
  const reads = []
  const unexpected = []
  const check = (condition, message) => { if (!condition) throw new Error(message) }
  let allowCreateReceipt = false
  let cancelRequests = 0
  let allowCancel = false
  let failNextProfile = false
  let authorization = ''

  // 1. 预置任务让登录时即可看到已有预占。模拟创建回执，状态、取消和正常资料请求走真实后端。
  await page.context().route('**/*', async route => {
    const request = route.request()
    const { origin, pathname: path } = new URL(request.url())
    if (origin !== baseURL) return route.abort()
    const method = request.method()
    if (method === 'GET') {
      if (path === taskPath || path === profilePath) reads.push(path)
      if (path === profilePath && failNextProfile) {
        failNextProfile = false
        return route.fulfill({ status: 503, json: { error: { message: '资料服务暂不可用。' } } })
      }
      return route.continue()
    }
    if (method === 'POST' && /^\/api\/v1\/auth\/(login|refresh)$/.test(path)) return route.continue()
    if (method === 'POST' && path === uploads && allowCreateReceipt) {
      allowCreateReceipt = false
      return route.fulfill({ status: 201, json: { data: {
        task: { id: '81', parent_id: '33', size_bytes: 3, status: 'UPLOADING', upload_mode: 'SINGLE' },
        upload_mode: 'SINGLE', content_path: `${taskPath}/content`,
      } } })
    }
    if (method === 'POST' && path === cancelPath && allowCancel) {
      allowCancel = false
      cancelRequests++
      authorization = request.headers().authorization ?? ''
      return route.continue()
    }
    unexpected.push(`${method} ${path}`)
    return route.abort()
  })

  const waitForResponse = (method, path) => page.waitForResponse(response =>
    new URL(response.url()).pathname === path && response.request().method() === method)
  const profileRows = page.locator('.auth-profile > div')
  const profileBytes = (label, bytes) => profileRows.filter({ has: page.getByText(label, { exact: true }) })
    .getByText(`${bytes} B`, { exact: true })
  await page.getByLabel('用户名', { exact: true }).fill('fixture_web_login')
  await page.getByLabel('密码', { exact: true }).fill('fixture-web-login-secret')
  await page.getByRole('button', { name: /^登\s*录$/ }).click()
  await profileBytes('预占空间', 5).waitFor()
  await profileBytes('已用空间', 0).waitFor()

  await page.getByRole('button', { name: '选择上传文件', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '准备单文件上传' })
  const button = name => dialog.getByRole('button', { name, exact: true })
  const status = label => dialog.getByRole('status').getByText(label, { exact: true })
  await dialog.getByLabel('选择本地文件', { exact: true }).evaluate(input => {
    const files = new DataTransfer()
    files.items.add(new File([new Uint8Array([1, 2, 3])], '隔离配额取消.bin'))
    input.files = files.files
    input.dispatchEvent(new Event('change', { bubbles: true }))
  })
  allowCreateReceipt = true
  await button('创建上传任务').click()
  await button('取消上传任务').click()
  const accepted = waitForResponse('POST', cancelPath)
  allowCancel = true
  await button('确认取消上传').click()
  const cancellation = await accepted
  check(cancellation.status() === 202 && (await cancellation.json()).data.status === 'CANCELLING', '真实取消应先返回 202')
  await status('取消中').waitFor()
  check(JSON.stringify(reads) === JSON.stringify([profilePath]) && await profileBytes('预占空间', 5).count() === 1,
    '取消受理不能提前读取或修改显示的预占')

  // 2. 仅测试探针等待正式 worker，页面保持手动查询且不轮询。
  check(authorization.startsWith('Bearer '), '等待探针须使用隔离会话')
  const deadline = Date.now() + 45000
  let cancelled = false
  do {
    const probe = await page.request.get(`${baseURL}${taskPath}`, {
      headers: { Authorization: authorization }, maxRedirects: 0, timeout: 2000,
    })
    check(probe.status() === 200, '只读探针应能查询当前任务')
    const { data } = await probe.json()
    await probe.dispose()
    check(data.status === 'CANCELLING' || data.status === 'CANCELLED', '后台应保持取消状态或提交终态')
    cancelled = data.status === 'CANCELLED'
    if (cancelled) break
    await page.waitForTimeout(300)
  } while (Date.now() < deadline)
  check(cancelled && JSON.stringify(reads) === JSON.stringify([profilePath]), '后台应完成收尾，页面在手动查询前保持原状')

  // 3. 首次资料读取失败，任务仍显示已取消。第二次查询应读回服务器配额。
  failNextProfile = true
  const failed = waitForResponse('GET', profilePath)
  await button('查询任务状态').click()
  check((await failed).status() === 503, '应交付模拟资料读取失败')
  await status('已取消').waitFor()
  const quotaError = dialog.getByRole('alert').getByText(/无法刷新配额.*可再次查询任务状态/)
  await quotaError.waitFor()
  check(await profileBytes('预占空间', 5).count() === 1 && await button('确认取消上传').count() === 0,
    '资料失败应保留旧数值和任务终态')

  const retried = waitForResponse('GET', profilePath)
  await button('查询任务状态').click()
  const profileResponse = await retried
  const { data: profile } = await profileResponse.json()
  check(profileResponse.status() === 200 && profile.quota_bytes === 1024 && profile.reserved_bytes === 2 && profile.used_bytes === 0,
    '服务端应只返还目标任务的预占')
  await quotaError.waitFor({ state: 'hidden' })
  await status('已取消').waitFor()
  await button('关闭').click()
  await dialog.waitFor({ state: 'hidden' })
  await profileBytes('预占空间', 2).waitFor()
  await profileBytes('已用空间', 0).waitFor()
  check(cancelRequests === 1 && unexpected.length === 0 &&
    JSON.stringify(reads) === JSON.stringify([profilePath, taskPath, profilePath, taskPath, profilePath]),
  '每次手动查到终态后只读一次本人资料，且没有重复写入')
  return 'CN533 browser checks passed'
}
