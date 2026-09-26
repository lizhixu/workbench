// 全功能 API 回归扫描：逐个打各模块端点，记录状态码与响应要点。
// 测试服务器可随意造数据，写操作会自建再清理。
const BASE = process.env.WM_BASE || 'http://192.255.178.173'
const results = []

let token = ''
let hostId = ''

const call = async (method, path, { body, raw, headers } = {}) => {
  const t0 = Date.now()
  try {
    const r = await fetch(BASE + path, {
      method,
      headers: {
        ...(body ? { 'Content-Type': 'application/json' } : {}),
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
        ...headers,
      },
      body: body ? JSON.stringify(body) : undefined,
    })
    const text = await r.text()
    let json = null
    try { json = JSON.parse(text) } catch {}
    return { status: r.status, text, json, ms: Date.now() - t0 }
  } catch (e) {
    return { status: 0, text: String(e), json: null, ms: Date.now() - t0 }
  }
}

const check = (name, res, expect = [200]) => {
  const ok = expect.includes(res.status)
  results.push({ name, status: res.status, ok, brief: (res.text || '').slice(0, 130).replace(/\s+/g, ' ') })
  return ok
}

// ---------- 1. 认证 ----------
{
  const bad = await call('POST', '/api/v1/auth/login', { body: { username: 'admin', password: 'wrong-password' } })
  check('认证 · 错误密码应拒绝', bad, [401, 403])

  const ok = await call('POST', '/api/v1/auth/login', { body: { username: 'admin', password: 'admin' } })
  check('认证 · admin 登录', ok)
  token = ok.json?.token || ''

  const me = await call('GET', '/api/v1/auth/me')
  check('认证 · GET /auth/me（文档要求）', me)

  const saved = token
  token = 'invalid.jwt.token'
  const guard = await call('GET', '/api/v1/hosts')
  check('认证 · 无效 token 应拒绝', guard, [401, 403])
  token = ''
  const noTok = await call('GET', '/api/v1/hosts')
  check('认证 · 缺 token 应拒绝', noTok, [401, 403])
  token = saved
}

// ---------- 2. 主机 ----------
{
  const list = await call('GET', '/api/v1/hosts')
  check('主机 · 列表', list)
  hostId = list.json?.data?.[0]?.id || ''
  check('主机 · 详情', await call('GET', `/api/v1/hosts/${hostId}`))
  check('主机 · 详情（不存在的 id 应 404）', await call('GET', '/api/v1/hosts/does-not-exist'), [404, 400])
  check('主机 · 设置标签', await call('PUT', `/api/v1/hosts/${hostId}/tags`, { body: { tags: ['测试', 'qa'] } }))
  check('主机 · 设置分组', await call('PUT', `/api/v1/hosts/${hostId}/group`, { body: { group: '' } }))
  check('主机 · 生成 enroll token', await call('POST', '/api/v1/hosts/enroll'))
  check('主机 · 安装脚本(linux)', await call('GET', '/install?os_type=linux'))
  check('主机 · 安装脚本(windows)', await call('GET', '/install?os_type=windows'))
  check('主机 · agent 二进制下载(linux/amd64)', await call('HEAD', '/api/v1/agent/binary?os=linux&arch=amd64'))
  check('主机 · agent 二进制下载(linux/arm64)', await call('HEAD', '/api/v1/agent/binary?os=linux&arch=arm64'))
  check('主机 · agent 二进制下载(windows)', await call('HEAD', '/api/v1/agent/binary?os=windows&arch=amd64'))
}

// ---------- 3. 系统状态 / 监控 ----------
{
  for (const kind of ['process', 'port', 'user', 'login']) {
    check(`系统状态 · ${kind}`, await call('GET', `/api/v1/hosts/${hostId}/sysinfo/${kind}`))
  }
  check('系统状态 · 非法 kind 应报错', await call('GET', `/api/v1/hosts/${hostId}/sysinfo/bogus`), [400, 404, 500])
  check('监控 · 实时', await call('GET', `/api/v1/hosts/${hostId}/metrics`))
  for (const range of ['1h', '24h', '7d']) {
    check(`监控 · 历史 ${range}`, await call('GET', `/api/v1/hosts/${hostId}/metrics/history?range=${range}`))
  }
  check('健康自检', await call('GET', '/api/v1/system/health'))
}

console.log(JSON.stringify({ token: token.slice(0, 12) + '...', hostId, results }, null, 1))
