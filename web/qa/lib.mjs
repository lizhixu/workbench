// 共享测试工具：登录、请求、断言、结果汇总。
const BASE = process.env.WM_BASE || 'http://192.255.178.173'

export const ctx = { token: '', hostId: '', results: [] }

export const call = async (method, path, { body, headers, rawBody, timeoutMs = 25000 } = {}) => {
  try {
    const r = await fetch(BASE + path, {
      method,
      headers: {
        ...(body ? { 'Content-Type': 'application/json' } : {}),
        ...(ctx.token ? { Authorization: `Bearer ${ctx.token}` } : {}),
        ...headers,
      },
      body: rawBody ?? (body ? JSON.stringify(body) : undefined),
      signal: AbortSignal.timeout(timeoutMs),
    })
    const text = await r.text()
    let json = null
    try { json = JSON.parse(text) } catch {}
    return { status: r.status, text, json }
  } catch (e) {
    return { status: 0, text: 'REQUEST_FAILED: ' + String(e?.message || e), json: null }
  }
}

// 逐条打印，避免中途卡死时丢掉已有结果
const emit = (r) => {
  const detail = r.status === '--' || !r.ok ? '   << ' + String(r.brief).slice(0, 150) : ''
  console.log(`${r.ok ? 'PASS' : 'FAIL'}  ${String(r.status).padEnd(5)}${r.name}${detail}`)
}

export const check = (name, res, expect = [200]) => {
  const ok = expect.includes(res.status)
  const r = { name, status: res.status, ok, brief: (res.text || '').slice(0, 160).replace(/\s+/g, ' ') }
  ctx.results.push(r)
  emit(r)
  return ok
}

export const note = (name, detail) => {
  const r = { name, status: '--', ok: true, brief: detail }
  ctx.results.push(r)
  emit(r)
}

export const fail = (name, detail) => {
  const r = { name, status: 'XX', ok: false, brief: detail }
  ctx.results.push(r)
  emit(r)
}

export const setup = async () => {
  const login = await call('POST', '/api/v1/auth/login', { body: { username: 'admin', password: 'admin' } })
  ctx.token = login.json?.token || ''
  if (!ctx.token) throw new Error('login failed: ' + login.text)
  const hosts = await call('GET', '/api/v1/hosts')
  const list = hosts.json?.data || []
  // 测试环境里可能注册了多台主机，其中只有一台装了 Docker。优先挑
  // hostname=serv 那台，否则回退到第一台在线主机，避免用例误报。
  const preferred = list.find((h) => h.hostname === 'serv' && h.status === 'online')
  const online = list.find((h) => h.status === 'online')
  ctx.hostId = (preferred || online || list[0])?.id || ''
  if (!ctx.hostId) throw new Error('no host registered')
  return ctx
}

export const report = () => {
  const bad = ctx.results.filter((r) => !r.ok)
  console.log(`\n合计 ${ctx.results.length} 项，失败 ${bad.length} 项`)
}

export { BASE }
