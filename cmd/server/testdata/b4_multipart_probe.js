async page => {
  return page.evaluate(async () => {
    const planResponse = await fetch('/plan')
    if (!planResponse.ok) throw new Error('读取合成分片计划失败')
    const plan = await planResponse.json()
    const target = new URL(plan.url)
    if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(target.origin) || target.origin === location.origin) {
      throw new Error('分片探测要求两个不同的回环来源')
    }

    // 1. 使用固定长度 Blob，由浏览器生成已签名的 Content-Length。
    const body = new Blob([new Uint8Array(plan.size).fill(plan.fill)])
    const response = await fetch(target, {
      method: 'PUT', credentials: 'omit', redirect: 'error', mode: 'cors',
      headers: { 'x-amz-checksum-sha256': plan.checksum }, body,
    })
    if (response.status !== 200) throw new Error(`跨域分片上传返回 HTTP ${response.status}`)

    // 2. 从页面可见的跨域响应读取 ETag，再回传它用于服务端核对。
    const etag = response.headers.get('etag')
    if (!etag) throw new Error('浏览器未读到跨域 ETag')
    const receipt = await fetch('/receipt', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(etag),
    })
    if (receipt.status !== 204) throw new Error('回传浏览器 ETag 失败')
    return 'CN550 browser ETag received'
  })
}
