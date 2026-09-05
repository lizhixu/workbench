// 复测：用户密码流程、分组授权级别、凭据金库（修正字段名/路径后）
import { call, check, fail, note, setup, report, ctx } from './lib.mjs'

await setup()
const H = ctx.hostId
const stamp = Date.now()
const uname = 'qa2_' + stamp
const pw1 = 'Qa#12345678'
const pw2 = 'Qa#87654321'

// ---------- 用户密码全流程 ----------
const created = await call('POST', '/api/v1/users', { body: { username: uname, password: pw1, role: 'operator' } })
check('用户 · 新增', created, [200, 201])
if (/"password_hash":"\$2[aby]\$/.test(created.text)) {
  fail('用户 · 响应不得回传口令哈希', created.text.slice(0, 120))
} else {
  note('用户 · 响应未回传口令哈希明文', '仅有空的 password_hash 字段（建议改 json:"-"）')
}

const login1 = await call('POST', '/api/v1/auth/login', { body: { username: uname, password: pw1 } })
check('用户 · 用初始口令登录', login1)
check('用户 · 管理员重置口令（new_password）', await call('POST', `/api/v1/users/${uname}/reset-password`, { body: { new_password: pw2 } }))
check('用户 · 旧口令应失效', await call('POST', '/api/v1/auth/login', { body: { username: uname, password: pw1 } }), [401])
const login2 = await call('POST', '/api/v1/auth/login', { body: { username: uname, password: pw2 } })
check('用户 · 新口令可登录', login2)

// operator 角色的权限边界
const adminToken = ctx.token
if (login2.json?.token) {
  ctx.token = login2.json.token
  check('权限 · operator 可读主机', await call('GET', '/api/v1/hosts'))
  const ex = await call('POST', `/api/v1/hosts/${H}/exec`, { body: { shell: 'bash', command: 'id -un' } })
  note('权限 · operator 执行命令', `状态 ${ex.status}`)
  check('权限 · operator 不能建用户', await call('POST', '/api/v1/users', { body: { username: 'x' + stamp, password: pw1, role: 'admin' } }), [401, 403])
  check('权限 · operator 不能读凭据金库', await call('GET', '/api/v1/vault/credentials'), [401, 403])
  check('权限 · operator 不能删主机', await call('DELETE', `/api/v1/hosts/${H}`), [401, 403])
  ctx.token = adminToken
}
check('用户 · 自助改密（老密码校验）', await call('PUT', `/api/v1/users/${uname}/password`, { body: { old_password: 'wrong', new_password: 'Qa#00000000' } }), [400, 401, 403])
check('用户 · 删除', await call('DELETE', `/api/v1/users/${uname}`))

// 弱口令
const weak = await call('POST', '/api/v1/users', { body: { username: 'qa_w_' + stamp, password: '1', role: 'viewer' } })
if ([200, 201].includes(weak.status)) {
  fail('用户 · 弱口令应被拒绝', `口令 "1" 被接受（状态 ${weak.status}），缺少最小长度/复杂度校验`)
  await call('DELETE', `/api/v1/users/qa_w_${stamp}`)
} else {
  check('用户 · 弱口令应被拒绝', weak, [400, 422])
}

// ---------- 分组授权（正确级别 operate/view）----------
const g = await call('POST', '/api/v1/groups', { body: { name: 'QA2 分组 ' + stamp } })
const gid = g.json?.data?.id || g.json?.id
check('分组 · 新增', g, [200, 201])
check('分组 · 授权用户 operate', await call('POST', `/api/v1/groups/${gid}/users`, { body: { username: 'admin', role: 'operate' } }))
check('分组 · 授权用户 view', await call('POST', `/api/v1/groups/${gid}/users`, { body: { username: 'admin', role: 'view' } }))
check('分组 · 组内用户', await call('GET', `/api/v1/groups/${gid}/users`))
check('分组 · 移除授权', await call('DELETE', `/api/v1/groups/${gid}/users/admin`))
check('分组 · 删除', await call('DELETE', `/api/v1/groups/${gid}`))

// ---------- 凭据金库（正确路径 /vault/credentials）----------
check('凭据金库 · 列表', await call('GET', '/api/v1/vault/credentials'))
const cred = await call('POST', '/api/v1/vault/credentials', {
  body: { name: 'QA 凭据 ' + stamp, type: 'password', username: 'root', secret: 'p@ssw0rd-' + stamp, host_id: H },
})
check('凭据金库 · 新增', cred, [200, 201])
const cid = cred.json?.data?.id || cred.json?.id
if (/p@ssw0rd-/.test(cred.text)) fail('凭据金库 · 新增响应不得回传密钥明文', cred.text.slice(0, 140))
else note('凭据金库 · 新增响应未回传密钥', '已脱敏')
const one = await call('GET', `/api/v1/vault/credentials/${cid}`)
check('凭据金库 · 详情', one)
if (/p@ssw0rd-/.test(one.text)) fail('凭据金库 · 详情不得回传密钥明文', one.text.slice(0, 140))
else note('凭据金库 · 详情未回传密钥', '已脱敏')
check('凭据金库 · 修改', await call('PUT', `/api/v1/vault/credentials/${cid}`, { body: { name: 'QA 凭据改名', type: 'password', username: 'root' } }))
check('凭据金库 · 删除', await call('DELETE', `/api/v1/vault/credentials/${cid}`))

report()
