// 验证「断线重连续接同一 PTY」：
//  1. 开会话，跑一条命令并在 shell 里留下可验证的状态（变量 + 后台进程）
//  2. 硬断开 WebSocket（不发 close 帧，模拟拔网线）
//  3. 宽限期内用同一 sid 重连，检查变量和进程是否还在
//  4. 再开一个会话，等过宽限期后重连，应连不上（PTY 已回收）
import WebSocket from 'ws'

const BASE = 'http://192.255.178.173'
const login = await fetch(`${BASE}/api/v1/auth/login`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ username: 'admin', password: 'admin' }),
})
const token = (await login.json()).token
const hostsRes = await fetch(`${BASE}/api/v1/hosts`, { headers: { Authorization: `Bearer ${token}` } })
const hostId = (await hostsRes.json()).data[0].id

const openSession = async () => {
  const r = await fetch(`${BASE}/api/v1/hosts/${hostId}/terminals`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
    body: JSON.stringify({ shell: 'bash', cols: 100, rows: 30 }),
  })
  const j = await r.json()
  return { sid: j.session_id, wsUrl: `${BASE.replace(/^http/, 'ws')}${j.ws_url}?token=${encodeURIComponent(token)}` }
}

const b64 = (s) => Buffer.from(s).toString('base64')

// 连上并收集输出，直到匹配 until 或超时
const attach = (wsUrl, { send = [], until, timeoutMs = 12000 }) =>
  new Promise((resolve) => {
    const ws = new WebSocket(wsUrl, { origin: BASE })
    let out = ''
    let done = false
    const finish = (why) => {
      if (done) return
      done = true
      resolve({ ws, out, why })
    }
    const timer = setTimeout(() => finish('timeout'), timeoutMs)
    ws.on('open', async () => {
      for (const s of send) {
        ws.send(JSON.stringify({ type: 'input', sid: 'x', data: b64(s) }))
        await new Promise((r) => setTimeout(r, 400))
      }
    })
    ws.on('message', (raw) => {
      let m
      try { m = JSON.parse(raw.toString()) } catch { return }
      if (m.type === 'output' && m.data) out += Buffer.from(m.data, 'base64').toString('utf8')
      if (until && until.test(out)) { clearTimeout(timer); finish('matched') }
    })
    ws.on('close', (c) => { clearTimeout(timer); finish('closed:' + c) })
    ws.on('error', (e) => { clearTimeout(timer); finish('error:' + e.message) })
  })

console.log('=== 场景一：宽限期内重连，应续接原 PTY ===')
const s1 = await openSession()
const marker = 'RC' + Date.now()
// 在 shell 里设一个变量，并起一个后台进程
const a1 = await attach(s1.wsUrl, {
  send: [`MYVAR=${marker}\n`, `sleep 600 & echo STARTED_$MYVAR\n`],
  until: new RegExp(`STARTED_${marker}`),
})
console.log(`  首连: ${a1.why}, 已设置变量与后台进程`)

// 硬断开：terminate() 不发 close 帧，等价于拔网线
a1.ws.terminate()
console.log('  已硬断开（未发 close 帧）')

await new Promise((r) => setTimeout(r, 5000))
// 用同一 sid 重连
const a2 = await attach(s1.wsUrl, {
  send: [`echo VAR_IS=$MYVAR; jobs | head -1\n`],
  until: new RegExp(`VAR_IS=${marker}`),
})
const varAlive = new RegExp(`VAR_IS=${marker}`).test(a2.out)
const jobAlive = /sleep 600/.test(a2.out)
console.log(`  重连: ${a2.why}`)
console.log(`  shell 变量保留: ${varAlive ? '是' : '否'}`)
console.log(`  后台进程保留: ${jobAlive ? '是' : '否'}`)
console.log(`  输出尾部: ${a2.out.trim().split('\n').slice(-3).join(' | ').slice(0, 160)}`)
a2.ws.close()

console.log('\n=== 场景二：超过宽限期（65s）后重连，PTY 应已回收 ===')
const s2 = await openSession()
const a3 = await attach(s2.wsUrl, { send: ['echo SESSION2_READY\n'], until: /SESSION2_READY/ })
console.log(`  首连: ${a3.why}`)
a3.ws.terminate()
console.log('  已硬断开，等待 65 秒让宽限期过期...')
await new Promise((r) => setTimeout(r, 65000))

const probe = await fetch(`${BASE}/api/v1/hosts/${hostId}/terminals`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
  body: JSON.stringify({ shell: 'bash' }),
})
console.log(`  开新会话仍可用: HTTP ${probe.status}`)
const a4 = await attach(s2.wsUrl, { timeoutMs: 8000 })
console.log(`  用过期 sid 重连: ${a4.why}（期望 closed:1006 或 error，说明会话已注销）`)
try { a4.ws.close() } catch {}
process.exit(0)
