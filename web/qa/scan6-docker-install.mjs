// 一键安装 Docker（走产品自身的接口），随后复测 Docker 与应用市场
import { call, check, note, fail, setup, report, ctx } from './lib.mjs'

await setup()
const H = ctx.hostId

const inst = await call('POST', `/api/v1/hosts/${H}/docker/install-script`, { timeoutMs: 600000 })
check('Docker · 一键安装', inst)
note('Docker · 安装输出尾部', (inst.text || '').slice(-220))

for (const p of ['ps', 'images', 'all']) {
  check(`Docker · ${p}`, await call('GET', `/api/v1/hosts/${H}/docker/${p}`, { timeoutMs: 60000 }))
}
check('应用市场 · 已装列表', await call('GET', `/api/v1/hosts/${H}/apps`, { timeoutMs: 60000 }))

report()
