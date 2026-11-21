async page => {
  const baseURL = new URL(page.url()).origin
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(baseURL)) throw new Error('验收只允许访问隔离回环服务')
  page.setDefaultTimeout(12000)
  const apiRequests = []
  const assert = (condition, message) => { if (!condition) throw new Error(message) }

  // 1. 允许合成登录和目录读取，阻止并记录任何意外业务写请求。
  await page.context().route('**/*', route => {
    const request = route.request()
    const url = new URL(request.url())
    if (url.origin !== baseURL) return route.abort()
    const method = request.method()
    const path = url.pathname
    if (path.startsWith('/api/')) apiRequests.push({ method, path, parentId: url.searchParams.get('parent_id') })
    if (method === 'GET' || (method === 'POST' && /^\/api\/v1\/auth\/(login|refresh)$/.test(path))) return route.continue()
    return route.abort()
  })

  await page.getByLabel('用户名', { exact: true }).fill('fixture_web_login')
  await page.getByLabel('密码', { exact: true }).fill('fixture-web-login-secret')
  await page.getByRole('button', { name: /^登\s*录$/ }).click()

  const dialog = page.getByRole('dialog', { name: '准备单文件上传' })
  const openDialog = () => page.getByRole('button', { name: '选择上传文件', exact: true }).click()
  const closeDialog = () => dialog.getByRole('button', { name: '关闭', exact: true }).click()
  const picker = dialog.getByLabel('选择本地文件', { exact: true })
  const chooseFile = (name, size) => picker.evaluate((input, fixture) => {
    const files = new DataTransfer()
    files.items.add(new File([new Uint8Array(fixture.size)], fixture.name))
    input.files = files.files
    input.dispatchEvent(new Event('change', { bubbles: true }))
  }, { name, size })
  const ready = dialog.getByRole('status').getByText('文件已就绪，尚未上传。')

  // 2. 验证单选入口、零字节、16 MiB 边界及清除无效声明。
  await openDialog()
  await dialog.getByText('根目录', { exact: true }).waitFor()
  assert(await picker.evaluate(input => !input.multiple), '入口应该只允许选一个文件')
  const requestCount = apiRequests.length
  const sizeLimit = 16 * 1024 * 1024
  await chooseFile('空文件.txt', 0)
  await ready.waitFor()
  await dialog.locator('dd').getByText('0 字节', { exact: true }).waitFor()
  await chooseFile('上限文件.bin', sizeLimit)
  await dialog.locator('dd').getByText('上限文件.bin', { exact: true }).waitFor()
  await dialog.locator('dd').getByText('16777216 字节', { exact: true }).waitFor()
  await chooseFile('超限文件.bin', sizeLimit + 1)
  await dialog.getByRole('alert').getByText('文件超过 16 MiB，请重新选择。').waitFor()
  await ready.waitFor({ state: 'hidden' })
  await chooseFile('COM¹.txt', 1)
  await dialog.getByRole('alert').getByText('文件名不符合命名规则，请重命名后重新选择。').waitFor()
  await chooseFile('重新选择.txt', 3)
  await dialog.locator('dd').getByText('重新选择.txt', { exact: true }).waitFor()
  await dialog.getByRole('alert').waitFor({ state: 'hidden' })
  assert(apiRequests.length === requestCount, '选择与替换文件不应发出 API 请求')
  await closeDialog()
  await dialog.waitFor({ state: 'hidden' })

  // 3. 进入真实子目录后重新打开，只显示新的目标和本次选中的合成文件。
  await page.getByRole('button', { name: '隔离上传选择目录', exact: true }).click()
  await openDialog()
  await dialog.getByText('根目录 / 隔离上传选择目录', { exact: true }).waitFor()
  assert(await ready.count() === 0, '根目录的待选文件不应留在子目录')
  await chooseFile('课程资料.MD', 5)
  await ready.waitFor()
  await dialog.locator('dd').getByText('课程资料.MD', { exact: true }).waitFor()
  await closeDialog()

  assert(apiRequests.length === requestCount + 1 &&
    apiRequests.some(request => request.method === 'GET' && request.path === '/api/v1/entries' && request.parentId === '33') &&
    apiRequests.at(-1).method === 'GET' && apiRequests.at(-1).path === '/api/v1/entries' && apiRequests.at(-1).parentId === '54',
  '切换目录时仅应以正确 ID 读取子目录')
  assert(!apiRequests.some(({ method, path }) => path.startsWith('/api/v1/uploads') ||
    (method !== 'GET' && !path.startsWith('/api/v1/auth/'))), '不得发送上传请求或改写业务数据')
  return 'CN522 browser checks passed'
}
