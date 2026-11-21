async page => {
  const baseURL = new URL(page.url()).origin
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(baseURL)) throw new Error('验收只允许访问隔离回环服务')
  page.setDefaultTimeout(12000)
  const rasterPath = '/api/v1/entries/64/download'
  const textPath = '/api/v1/entries/65/download'
  const filename = '合成预览.png'
  let nextAuthorization = ''
  let nextImage
  let authorizations = 0
  let imageRequests = 0
  let unexpected = 0
  const check = (condition, message) => { if (!condition) throw new Error(message) }

  // 1. 放行明确发起的授权和签名 GET，其他跨源请求均拦截。
  await page.context().route('**/*', async route => {
    const request = route.request()
    const { origin, pathname } = new URL(request.url())
    const method = request.method()
    if (origin !== baseURL) {
      if (method !== 'GET' || request.url() !== nextImage?.url) {
        unexpected++
        return route.abort()
      }
      const action = nextImage
      nextImage = undefined
      imageRequests++
      const headers = request.headers()
      check(request.resourceType() === 'image' && !headers.authorization && !headers.referer,
        '浏览器应以图片请求原样读取签名地址，且不附带令牌或来源')
      if (action.fail) {
        return route.fulfill({ status: 503, headers: { 'Cache-Control': 'no-store' }, body: 'fixture image failure' })
      }
      if (action.gate) await action.gate.promise
      return route.continue()
    }
    if (method === 'GET' || (method === 'POST' && /^\/api\/v1\/auth\/(login|refresh)$/.test(pathname))) {
      return route.continue()
    }
    if (method === 'POST' && pathname === nextAuthorization) {
      nextAuthorization = ''
      check(request.postDataJSON().purpose === 'preview', '展示图片只应使用预览授权')
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
  const showImage = dialog.getByRole('button', { name: '查看图片', exact: true })
  const refresh = dialog.getByRole('button', { name: '重新获取预览链接', exact: true })
  const image = dialog.getByRole('img', { name: filename, exact: true })
  const openImage = page.getByRole('button', { name: '获取预览链接 ' + filename, exact: true })

  async function authorize(button, path = rasterPath) {
    const issued = page.waitForResponse(response => new URL(response.url()).pathname === path &&
      response.request().method() === 'POST')
    nextAuthorization = path
    await button.click()
    const response = await issued
    check(response.status() === 200, '隔离服务应签发真实预览授权')
    const { data } = await response.json()
    await dialog.getByLabel('短期预览链接', { exact: true }).waitFor()
    if (path === rasterPath) {
      const signed = new URL(data.url)
      check(signed.protocol === 'http:' && signed.hostname === '127.0.0.1' &&
        signed.pathname.endsWith('/preview-fixture/64') && data.mime_type === 'image/png' &&
        signed.searchParams.get('response-content-type') === 'image/png' && !!signed.searchParams.get('X-Amz-Signature'),
      '应读取本轮隔离 PNG 的签名地址')
      await showImage.waitFor()
    }
    return data
  }

  async function verifyImage(grant, gate) {
    nextImage = { url: grant.url, gate }
    const loaded = page.waitForResponse(response => response.url() === grant.url && response.request().method() === 'GET')
    await showImage.click()
    if (gate) {
      await dialog.getByRole('status').getByText('正在加载图片…').waitFor()
      gate.resolve()
    }
    check((await loaded).status() === 200, '真实存储应返回 PNG')
    await image.waitFor()
    check(await image.evaluate((element, source) => element.getAttribute('src') === source && element.complete &&
      element.naturalWidth === 1 && element.naturalHeight === 1, grant.url), '应在窗口内解码实际的 1×1 图片')
  }

  // 2. 到期点击拦截 GET，重新授权后再由用户主动加载。
  const expired = await authorize(openImage)
  check(imageRequests === 0, '只取得授权时不应读取图片')
  await page.clock.setFixedTime(new Date(expired.expires_at))
  await showImage.click()
  await dialog.getByRole('alert').getByText('预览链接已过期。').waitFor()
  check(imageRequests === 0 && authorizations === 1, '过期点击不得发出 GET 或自动重新授权')
  await page.clock.setFixedTime(new Date())
  const loaded = await authorize(refresh)
  await verifyImage(loaded, Promise.withResolvers())
  await dialog.getByRole('button', { name: '关闭', exact: true }).click()
  await dialog.waitFor({ state: 'hidden' })

  // 3. 模拟一次读取失败，校验重新获取后可以恢复显示。
  // 签名按秒生成，在故障注入前跨过签名秒，确保浏览器发出新的图片请求。
  await page.waitForTimeout(1000)
  const failed = await authorize(openImage)
  check(failed.url !== loaded.url, '模拟错误前须签发不同的链接')
  nextImage = { url: failed.url, fail: true }
  await showImage.click()
  await dialog.getByRole('alert').getByText('图片加载失败。').waitFor()
  check(imageRequests === 2 && authorizations === 3 && await image.count() === 0, '读取失败时不得显示图片或自动重试')
  await page.waitForTimeout(1000)
  const renewed = await authorize(refresh)
  check(renewed.url !== failed.url, '恢复时须签发不同的链接')
  await verifyImage(renewed)
  await dialog.getByRole('button', { name: '关闭', exact: true }).click()
  await dialog.waitFor({ state: 'hidden' })

  await authorize(page.getByRole('button', { name: '获取预览链接 合成预览.txt', exact: true }), textPath)
  check(await showImage.count() === 0 && imageRequests === 3 && authorizations === 5 && unexpected === 0,
    '纯文本不应走栅格图片展示，总请求数须符合点击次数')
  return 'CN530 browser checks passed'
}
