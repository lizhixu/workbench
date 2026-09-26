// 最终复核：本次修复项的回归确认
import { call, check, note, fail, setup, report, ctx } from './lib.mjs'

await setup()
const H = ctx.hostId
const stamp = Date.now()

// 1. /auth/me 与 /auth/logout（文档要求，之前缺失）
const me = await call('GET', '/api/v1/auth/me')
check('认证 · /auth/me', me)
if (/password_hash|\$2[aby]\$/.test(me.text)) fail('认证 · /auth/me 不得含口令哈希', me.text.slice(0, 120))
else note('认证 · /auth/me 内容', me.text.slice(0, 110))
check('认证 · /auth/logout', await call('POST', '/api/v1/auth/logout'))

// 2. 凭据金库：详情不回传明文 + 删除后仍可用
const cred = await call('POST', '/api/v1/vault/credentials', {
  body: { name: 'QA 回归 ' + stamp, type: 'password', username: 'root', secret: 'SECRET-' + stamp },
})
check('金库 · 新增', cred, [200, 201])
const cid = cred.json?.data?.id
const one = await call('GET', `/api/v1/vault/credentials/${cid}`)
check('金库 · 详情', one)
if (/SECRET-/.test(one.text)) fail('金库 · 详情不得回传密钥明文', one.text.slice(0, 140))
else note('金库 · 详情已脱敏', one.text.slice(0, 120))
check('金库 · 删除', await call('DELETE', `/api/v1/vault/credentials/${cid}`))
check('金库 · 删除后列表仍可用（不再死锁）', await call('GET', '/api/v1/vault/credentials'))
check('金库 · 删除后仍可新增', await call('POST', '/api/v1/vault/credentials', {
  body: { name: 'QA 回归2 ' + stamp, type: 'password', secret: 'x-' + stamp },
}), [200, 201])

// 3. 文件上传（自锁死 + 乱序 + 413 三个问题）
const dir = '/tmp/wm-final-' + stamp
check('文件 · 建目录', await call('POST', `/api/v1/hosts/${H}/files/mkdir`, { body: { path: dir } }))
const big = Buffer.alloc(3 * 1024 * 1024, 'z')
const up = await fetch(`http://192.255.178.173/api/v1/hosts/${H}/files/upload?path=${encodeURIComponent(dir + '/3mb.bin')}`, {
  method: 'POST',
  headers: { Authorization: `Bearer ${ctx.token}`, 'Content-Type': 'application/octet-stream' },
  body: big,
})
check('文件 · 上传 3MB（原先 413 + 死锁）', { status: up.status, text: await up.text() })
const st = await call('GET', `/api/v1/hosts/${H}/files/stat?path=${encodeURIComponent(dir + '/3mb.bin')}`)
const size = st.json?.data?.size
if (size === big.length) note('文件 · 3MB 字节数一致', String(size))
else fail('文件 · 3MB 字节数一致', `期望 ${big.length} 实际 ${size}`)
check('文件 · 上传后列目录仍可用（不再污染子系统）', await call('GET', `/api/v1/hosts/${H}/files?path=${encodeURIComponent(dir)}`))
check('文件 · 不存在路径返回 404', await call('GET', `/api/v1/hosts/${H}/files?path=/no/such/x`), [404])
check('文件 · 清理', await call('DELETE', `/api/v1/hosts/${H}/files?path=${encodeURIComponent(dir)}`))

// 4. 权限：operator 不得解绑主机 / 改高危策略
const uname = 'qa_final_' + stamp
await call('POST', '/api/v1/users', { body: { username: uname, password: 'Qa#12345678', role: 'operator' } })
const lg = await call('POST', '/api/v1/auth/login', { body: { username: uname, password: 'Qa#12345678' } })
const adminToken = ctx.token
if (lg.json?.token) {
  ctx.token = lg.json.token
  check('权限 · operator 不能解绑主机', await call('DELETE', `/api/v1/hosts/${H}`), [403])
  check('权限 · operator 不能改高危策略', await call('PUT', '/api/v1/policy/command', { body: { enabled: false } }), [403])
  check('权限 · operator 不能删会话录像', await call('DELETE', '/api/v1/sessions/whatever'), [403])
  check('权限 · operator 仍可执行命令', await call('POST', `/api/v1/hosts/${H}/exec`, { body: { shell: 'bash', command: 'true' } }))
  ctx.token = adminToken
}
await call('DELETE', `/api/v1/users/${uname}`)
check('用户 · 弱口令仍被拒', await call('POST', '/api/v1/users', { body: { username: 'qa_w2_' + stamp, password: 'abc', role: 'viewer' } }), [400])

// 5. Docker 列表恒为数组
const ps = await call('GET', `/api/v1/hosts/${H}/docker/ps`, { timeoutMs: 60000 })
check('Docker · ps', ps)
if (Array.isArray(ps.json?.data)) note('Docker · ps 返回数组', `${ps.json.data.length} 个容器`)
else fail('Docker · ps 返回数组', typeof ps.json?.data)
const im = await call('GET', `/api/v1/hosts/${H}/docker/images`, { timeoutMs: 60000 })
if (Array.isArray(im.json?.data)) note('Docker · images 返回数组', `${im.json.data.length} 个镜像`)
else fail('Docker · images 返回数组', typeof im.json?.data)

// 6. 内置告警规则含上线 + 离线
const rules = await call('GET', '/api/v1/alerts/rules')
const types = (rules.json?.data || []).map((r) => r.type)
if (types.includes('online') && types.includes('offline')) note('告警 · 内置上线/离线规则齐全', types.join(','))
else fail('告警 · 内置上线/离线规则齐全', `现有类型 ${types.join(',') || '(空)'}`)

// 7. 主机内网 IP 不应是 Docker 网桥
const host = await call('GET', `/api/v1/hosts/${H}`)
const ip = host.json?.data?.internal_ip
if (ip && !ip.startsWith('172.17.')) note('主机 · 内网 IP 未取 Docker 网桥', String(ip))
else fail('主机 · 内网 IP 未取 Docker 网桥', String(ip))

report()
