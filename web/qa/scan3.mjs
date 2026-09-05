// 终端会话 / 录制回放 / 终端分享 / 告警
import { call, check, fail, note, setup, report, ctx, BASE } from './lib.mjs'
import WebSocket from 'ws'

await setup()
const H = ctx.hostId

// ---------- 终端会话 ----------
const open = await call('POST', `/api/v1/hosts/${H}/terminals`, { body: { shell: 'bash', cols: 100, rows: 30 } })
check('终端 · 开启会话', open)
const sid = open.json?.session_id
const wsUrl = `${BASE.replace(/^http/, 'ws')}${open.json?.ws_url}?token=${encodeURIComponent(ctx.token)}`

// 未带 token 的 WS 应被拒绝
await new Promise((resolve) => {
  const bad = new WebSocket(`${BASE.replace(/^http/, 'ws')}${open.json?.ws_url}`, { origin: BASE })
  let settled = false
  const done = (ok, why) => { if (!settled) { settled = true; ok ? note('终端 · 无 token 的 WS 被拒绝', why) : fail('终端 · 无 token 的 WS 被拒绝', why); try { bad.close() } catch {} ; resolve() } }
  bad.on('open', () => done(false, '竟然连上了'))
  bad.on('unexpected-response', (_, res) => done(true, `HTTP ${res.statusCode}`))
  bad.on('error', (e) => done(true, String(e.message).slice(0, 60)))
  setTimeout(() => done(false, '超时无响应'), 8000)
})

// 正常会话：执行命令并校验回显
let output = ''
await new Promise((resolve) => {
  const ws = new WebSocket(wsUrl, { origin: BASE })
  const timer = setTimeout(() => { try { ws.close() } catch {}; resolve() }, 14000)
  ws.on('open', () => {
    ws.send(JSON.stringify({ type: 'resize', sid, cols: 100, rows: 30 }))
    ws.send(JSON.stringify({ type: 'input', sid, data: Buffer.from('echo TERM_OK_$((6*7))\n').toString('base64') }))
  })
  ws.on('message', (raw) => {
    let m; try { m = JSON.parse(raw.toString()) } catch { return }
    if (m.type === 'output' && m.data) output += Buffer.from(m.data, 'base64').toString('utf8')
    if (output.includes('TERM_OK_42')) { clearTimeout(timer); try { ws.close() } catch {}; resolve() }
  })
  ws.on('error', () => { clearTimeout(timer); resolve() })
})
if (output.includes('TERM_OK_42')) note('终端 · PTY 执行并回显', 'TERM_OK_42')
else fail('终端 · PTY 执行并回显', `实际输出 ${JSON.stringify(output.slice(-120))}`)

// ---------- 终端分享 ----------
const share = await call('POST', `/api/v1/hosts/${H}/terminals/${sid}/share`, { body: { mode: 'view' } })
check('终端分享 · 生成分享令牌', share)
const shareToken = share.json?.share_token
const shareCode = share.json?.share_code
if (shareToken) {
  check('终端分享 · 令牌信息查询（免登录）', await call('GET', `/api/v1/terminals/share/${shareToken}`))
} else {
  fail('终端分享 · 令牌信息查询', `响应无 share_token 字段：${share.text.slice(0, 120)}`)
}
if (shareCode) {
  check('终端分享 · 6 位口令查询', await call('GET', `/api/v1/terminals/share/${shareCode}`))
} else {
  fail('终端分享 · 6 位口令', `响应无 share_code 字段：${share.text.slice(0, 120)}`)
}
check('终端分享 · 无效令牌应拒绝', await call('GET', '/api/v1/terminals/share/bogus-token-xxx'), [400, 401, 403, 404])

// ---------- 会话审计与录像 ----------
const sess = await call('GET', '/api/v1/sessions')
check('会话审计 · 列表', sess)
const list = sess.json?.data || []
const closed = list.find((s) => s.id !== sid) || list[0]
check('会话审计 · 单条详情', await call('GET', `/api/v1/sessions/${closed?.id}`))
const rec = await call('GET', `/api/v1/sessions/${closed?.id}/recording?token=${encodeURIComponent(ctx.token)}`)
check('会话审计 · 录像下载', rec)
if (rec.status === 200) {
  const firstLine = rec.text.split('\n')[0] || ''
  if (/"version"\s*:\s*2/.test(firstLine)) note('会话审计 · 录像为 asciicast v2', firstLine.slice(0, 90))
  else fail('会话审计 · 录像为 asciicast v2', `首行 ${firstLine.slice(0, 90)}`)
  const rows = rec.text.trim().split('\n').slice(1)
  if (rows.length > 0 && rows.every((l) => l.startsWith('['))) note('会话审计 · 录像事件行格式', `${rows.length} 行事件`)
  else fail('会话审计 · 录像事件行格式', `${rows.length} 行，样例 ${rows[0]?.slice(0, 60)}`)
}
check('会话审计 · 关闭当前会话', await call('DELETE', `/api/v1/sessions/${sid}`))

// ---------- 告警 ----------
check('告警 · 规则列表', await call('GET', '/api/v1/alerts/rules'))
const rule = await call('POST', '/api/v1/alerts/rules', {
  body: { name: 'QA CPU 阈值', type: 'cpu', threshold: 1, duration_sec: 10, severity: 'warning', enabled: true },
})
check('告警 · 新增规则', rule, [200, 201])
const rid = rule.json?.data?.id || rule.json?.id
check('告警 · 修改规则', await call('PUT', `/api/v1/alerts/rules/${rid}`, { body: { name: 'QA CPU 阈值改名', threshold: 99, enabled: false, type: 'cpu', severity: 'info', duration_sec: 30 } }))
check('告警 · 事件列表', await call('GET', '/api/v1/alerts/events'))
const ev = (await call('GET', '/api/v1/alerts/events')).json?.data || []
if (ev.length) {
  check('告警 · 单条 ack', await call('POST', `/api/v1/alerts/events/${ev[0].id}/ack`))
  check('告警 · 全部 ack', await call('POST', '/api/v1/alerts/events/ack-all'))
  check('告警 · 删除单条事件', await call('DELETE', `/api/v1/alerts/events/${ev[0].id}`))
} else {
  fail('告警 · 事件相关操作', '没有任何事件可测（内置上线/离线告警未产生事件？）')
}
check('告警 · webhook 读取', await call('GET', '/api/v1/alerts/webhook'))
check('告警 · webhook 设置', await call('PUT', '/api/v1/alerts/webhook', { body: { url: 'http://127.0.0.1:9/none', enabled: false } }))
check('告警 · 删除规则', await call('DELETE', `/api/v1/alerts/rules/${rid}`))
// 内置规则可删是刻意设计（store.seedBuiltinRules 只在缺同类型规则时补种），
// 这里只记录行为，删除后立刻补回，避免破坏测试环境的内置告警。
const delBuiltin = await call('DELETE', '/api/v1/alerts/rules/builtin-offline')
note('告警 · 内置规则允许删除（设计如此，重启后按类型补种）', `状态 ${delBuiltin.status}`)
if (delBuiltin.status === 200) {
  check('告警 · 手工补回离线规则', await call('POST', '/api/v1/alerts/rules', {
    body: { name: '主机离线告警', type: 'offline', severity: 'critical', enabled: true },
  }), [200, 201])
}

report()
