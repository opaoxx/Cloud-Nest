async page => {
  const baseURL = new URL(page.url()).origin
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(baseURL)) throw new Error('验收只允许访问隔离回环服务')
  page.setDefaultTimeout(12000)
  const grants = []
  let pending
  let unexpected = 0
  let outside = 0
  const check = (condition, message) => { if (!condition) throw new Error(message) }

  // 1. 使用真实隔离授权，拦截任何存储正文请求或意外写入。
  await page.context().route('**/*', async route => {
    const request = route.request()
    const { origin, pathname } = new URL(request.url())
    const method = request.method()
    if (origin !== baseURL) {
      outside++
      return route.abort()
    }
    if (method === 'GET' || (method === 'POST' && /^\/api\/v1\/auth\/(login|refresh)$/.test(pathname))) {
      return route.continue()
    }
    if (method === 'POST' && pathname === pending?.path) {
      const action = pending
      pending = undefined
      const body = request.postDataJSON()
      const headers = request.headers()
      check(body.purpose === action.purpose && body.expected_revision === action.revision &&
        Object.keys(body).length === 2 && (headers.authorization ?? '').startsWith('Bearer '),
      '授权须使用当前会话、列表版本和用户选择的用途')
      grants.push(body.purpose)
      if (action.badMime) {
        const result = await route.fetch({ maxRedirects: 0, timeout: 5000 })
        check(result.status() === 200, '隔离 PNG 对象应能签发预览授权')
        const payload = await result.json()
        payload.data.mime_type = 'text/html'
        return route.fulfill({ status: 200, json: payload })
      }
      return route.continue()
    }
    unexpected++
    return route.abort()
  })

  await page.getByLabel('用户名', { exact: true }).fill('fixture_web_login')
  await page.getByLabel('密码', { exact: true }).fill('fixture-web-login-secret')
  await page.getByRole('button', { name: /^登\s*录$/ }).click()
  const previewDialog = page.getByRole('dialog', { name: '预览授权' })
  const previewLink = previewDialog.getByLabel('短期预览链接', { exact: true })

  function previewButton(name) {
    return page.getByRole('button', { name: '获取预览链接 ' + name, exact: true })
  }

  async function requestGrant(button, id, revision, purpose, badMime = false) {
    const path = '/api/v1/entries/' + id + '/download'
    const issued = page.waitForResponse(response => {
      const url = new URL(response.url())
      return url.origin === baseURL && url.pathname === path && response.request().method() === 'POST'
    })
    pending = { path, revision, purpose, badMime }
    await button.click()
    return issued
  }

  async function verifyPreview(response, mimeType) {
    const { data } = await response.json()
    check(response.status() === 200 && data.mime_type === mimeType && data.disposition.startsWith('inline;') &&
      Date.parse(data.expires_at) > Date.now(), '应返回约定的内联授权和有效期')
    await previewLink.waitFor()
    check(await previewLink.evaluate((input, expected) => input.readOnly && input.value === expected, data.url),
      '页面须保留原短期预览链接')
    check(await previewDialog.getByRole('link', { name: '下载文件', exact: true }).count() === 0,
      '预览窗口应按回执用途显示')
    await previewDialog.getByRole('button', { name: '关闭', exact: true }).click()
    await previewDialog.waitFor({ state: 'hidden' })
  }

  // 2. 拒绝异常 MIME，手动重试后验证 PNG 与纯文本的真实 inline 回执。
  const corrupt = await requestGrant(previewButton('合成预览.png'), '64', '9007199254740995', 'preview', true)
  check(corrupt.status() === 200, '应模拟一次格式异常的授权回执')
  await previewDialog.getByRole('alert').getByText('服务返回的预览授权有误。').waitFor()
  check(await previewLink.count() === 0, '异常预览媒体类型不能展示链接')
  await verifyPreview(await requestGrant(
    previewDialog.getByRole('button', { name: '重新获取链接', exact: true }), '64', '9007199254740995', 'preview',
  ), 'image/png')
  await verifyPreview(await requestGrant(previewButton('合成预览.txt'), '65', '2', 'preview'), 'text/plain')

  // 3. 服务端拒绝不支持的预览，只有点击回退入口才申请下载授权。
  const denied = await requestGrant(previewButton('不支持预览.bin'), '66', '1', 'preview')
  check(denied.status() === 422 && (await denied.json()).error.code === 'UNSUPPORTED_PREVIEW', '应由真实后端拒绝不支持格式')
  await previewDialog.getByRole('alert').getByText('此文件暂不支持预览，可以改为下载。').waitFor()
  check(grants.length === 4 && await previewLink.count() === 0, '后端拒绝后不得自动申请下载')
  const fallback = await requestGrant(
    previewDialog.getByRole('button', { name: '改为获取下载链接', exact: true }), '66', '1', 'download',
  )
  const { data } = await fallback.json()
  check(fallback.status() === 200 && data.disposition.startsWith('attachment;'), '手动改为下载应得到附件回执')
  await page.getByRole('dialog', { name: '下载授权' }).getByLabel('短期下载链接', { exact: true }).waitFor()
  check(grants.join(',') === 'preview,preview,preview,preview,download' && unexpected === 0 && outside === 0,
    '全程只允许明确发起的授权，正文请求须为零')
  return 'CN529 browser checks passed'
}
