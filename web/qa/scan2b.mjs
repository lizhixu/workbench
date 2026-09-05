// 文件管理复测：上传走 ?path= + 原始 body（与 server/internal/api/files.go 实现一致）
import { call, check, fail, note, setup, report, ctx, BASE } from './lib.mjs'

await setup()
const H = ctx.hostId
const dir = '/tmp/wm-qa-' + Date.now()
const content = 'watchman-qa-' + Date.now() + '\n第二行中文内容\n'

check('文件 · 新建目录', await call('POST', `/api/v1/hosts/${H}/files/mkdir`, { body: { path: dir } }))

const up = await fetch(`${BASE}/api/v1/hosts/${H}/files/upload?path=${encodeURIComponent(dir + '/a.txt')}`, {
  method: 'POST',
  headers: { Authorization: `Bearer ${ctx.token}`, 'Content-Type': 'application/octet-stream' },
  body: Buffer.from(content),
})
const upText = await up.text()
check('文件 · 上传（?path= + raw body）', { status: up.status, text: upText })

const dl = await call('GET', `/api/v1/hosts/${H}/files/download?path=${encodeURIComponent(dir + '/a.txt')}`)
check('文件 · 下载', dl)
if (dl.text === content) note('文件 · 上传下载内容一致（含中文）', '完全一致')
else fail('文件 · 上传下载内容一致（含中文）', `实际 ${JSON.stringify(dl.text.slice(0, 60))}`)

check('文件 · 复制', await call('POST', `/api/v1/hosts/${H}/files/copy`, { body: { path: dir + '/a.txt', dest_path: dir + '/b.txt' } }))
check('文件 · 移动/重命名', await call('POST', `/api/v1/hosts/${H}/files/move`, { body: { path: dir + '/b.txt', dest_path: dir + '/c.txt' } }))

const ls = await call('GET', `/api/v1/hosts/${H}/files?path=${encodeURIComponent(dir)}`)
const names = (ls.json?.data || []).map((e) => e.name).sort().join(',')
if (names === 'a.txt,c.txt') note('文件 · 目录内容符合预期', names)
else fail('文件 · 目录内容符合预期', `期望 a.txt,c.txt 实际 "${names}"`)

// 大文件（2MB）分片与校验
const big = Buffer.alloc(2 * 1024 * 1024, 'w')
const upBig = await fetch(`${BASE}/api/v1/hosts/${H}/files/upload?path=${encodeURIComponent(dir + '/big.bin')}`, {
  method: 'POST',
  headers: { Authorization: `Bearer ${ctx.token}`, 'Content-Type': 'application/octet-stream' },
  body: big,
})
check('文件 · 上传 2MB 大文件', { status: upBig.status, text: await upBig.text() })
const statBig = await call('GET', `/api/v1/hosts/${H}/files/stat?path=${encodeURIComponent(dir + '/big.bin')}`)
const sz = statBig.json?.data?.size
if (sz === big.length) note('文件 · 大文件字节数一致', `${sz} bytes`)
else fail('文件 · 大文件字节数一致', `期望 ${big.length} 实际 ${sz}`)

// 单文件删除
check('文件 · 删除单个文件', await call('DELETE', `/api/v1/hosts/${H}/files?path=${encodeURIComponent(dir + '/c.txt')}`))
const ls2 = await call('GET', `/api/v1/hosts/${H}/files?path=${encodeURIComponent(dir)}`)
const names2 = (ls2.json?.data || []).map((e) => e.name).sort().join(',')
if (names2 === 'a.txt,big.bin') note('文件 · 单文件删除生效', names2)
else fail('文件 · 单文件删除生效', `实际 "${names2}"`)

// 目录递归删除 + 错误处理
check('文件 · 递归删除目录', await call('DELETE', `/api/v1/hosts/${H}/files?path=${encodeURIComponent(dir)}`))
const bad = await call('GET', `/api/v1/hosts/${H}/files?path=/no/such/dir`)
check('文件 · 不存在路径应 404 且带 agent 真实错误', bad, [404])
if (bad.status === 404 && /no such file/.test(bad.text)) note('文件 · 错误信息透传', bad.text.slice(0, 80))

report()
