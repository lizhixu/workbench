// 安全扫描 / 审计 / 备份恢复 / Docker / 应用市场 / Agent 升级 / AI 降级
import { call, check, fail, note, setup, report, ctx } from './lib.mjs'

await setup()
const H = ctx.hostId

// ---------- 安全扫描 ----------
check('扫描 · 任务列表', await call('GET', '/api/v1/scans'))
for (const type of ['baseline', 'intrusion', 'vulnerability']) {
  const r = await call('POST', `/api/v1/hosts/${H}/scans`, { body: { type }, timeoutMs: 120000 })
  check(`扫描 · 触发 ${type}`, r)
  if (r.status === 200) {
    const findings = r.json?.data?.findings ?? r.json?.findings
    note(`扫描 · ${type} 结果条数`, Array.isArray(findings) ? String(findings.length) : JSON.stringify(r.json).slice(0, 100))
  }
}
const scans = await call('GET', '/api/v1/scans')
const sid = (scans.json?.data || [])[0]?.id
if (sid) check('扫描 · 单任务详情', await call('GET', `/api/v1/scans/${sid}`))
else fail('扫描 · 单任务详情', '扫描列表为空')
check('扫描 · 主机最新结果', await call('GET', `/api/v1/hosts/${H}/scans/latest`))

// ---------- 审计 ----------
check('审计 · 列表', await call('GET', '/api/v1/audit'))
check('审计 · 按用户过滤', await call('GET', '/api/v1/audit?username=admin'))
check('审计 · 按动作过滤', await call('GET', '/api/v1/audit?action=exec'))
const exp = await call('GET', '/api/v1/audit/export')
check('审计 · 导出 CSV', exp)
if (exp.status === 200) {
  const head = (exp.text || '').split('\n')[0]
  if (/,/.test(head)) note('审计 · 导出为 CSV 表头', head.slice(0, 90))
  else fail('审计 · 导出为 CSV 表头', head.slice(0, 90))
}

// ---------- 备份恢复 ----------
check('备份 · 列表', await call('GET', '/api/v1/system/backups'))
const bk = await call('POST', '/api/v1/system/backup', { timeoutMs: 90000 })
check('备份 · 创建', bk, [200, 201])
const name = bk.json?.data?.name || bk.json?.name
const after = await call('GET', '/api/v1/system/backups')
const found = (after.json?.data || []).some((b) => (b.name || b) === name)
if (name && found) note('备份 · 新备份出现在列表', String(name))
else fail('备份 · 新备份出现在列表', `name=${name} 列表=${after.text.slice(0, 120)}`)
if (name) {
  const dl = await call('GET', `/api/v1/system/backups/${encodeURIComponent(name)}/download`, { timeoutMs: 90000 })
  check('备份 · 下载', dl)
  if (dl.status === 200) note('备份 · 下载字节数', String(dl.text.length))
  check('备份 · 删除', await call('DELETE', `/api/v1/system/backups/${encodeURIComponent(name)}`))
}
note('备份 · 恢复接口', '未实测（restore 会覆盖运行中数据，留作人工确认）')

// ---------- Docker ----------
const ps = await call('GET', `/api/v1/hosts/${H}/docker/ps`)
note('Docker · ps（未装 Docker 的降级行为）', `状态 ${ps.status} ${ps.text.slice(0, 90)}`)
const inst = await call('POST', `/api/v1/hosts/${H}/docker/install-script`)
note('Docker · 一键安装脚本', `状态 ${inst.status} ${inst.text.slice(0, 90)}`)

// ---------- 应用市场 ----------
const cat = await call('GET', '/api/v1/apps/catalog')
check('应用市场 · 目录', cat)
const apps = cat.json?.data || []
note('应用市场 · 应用数量', String(apps.length))
if (apps.length) note('应用市场 · 首个应用', JSON.stringify(apps[0]).slice(0, 120))
check('应用市场 · 已装列表', await call('GET', `/api/v1/hosts/${H}/apps`))

// ---------- Agent 升级 ----------
const up = await call('POST', `/api/v1/hosts/${H}/upgrade`, { body: { version: '0.0.0-nonexistent' } })
note('Agent 升级 · 非法版本的行为', `状态 ${up.status} ${up.text.slice(0, 120)}`)

// ---------- AI（未配置模型时应降级而非报 500）----------
const aiCfg = await call('GET', '/api/v1/ai/config')
check('AI · 读配置', aiCfg)
if (/api_key/.test(aiCfg.text) && !/"api_key":""/.test(aiCfg.text)) {
  fail('AI · 配置不得回传 api_key 明文', aiCfg.text.slice(0, 140))
} else {
  note('AI · 配置未回传 api_key', aiCfg.text.slice(0, 100))
}
for (const [name, path, body] of [
  ['自然语言转命令', '/api/v1/ai/nl2command', { host_id: H, prompt: '查看磁盘占用' }],
  ['对话问答', '/api/v1/ai/chat', { messages: [{ role: 'user', content: '当前有几台主机在线' }] }],
  ['智能诊断', '/api/v1/ai/diagnose', { host_id: H }],
  ['执行结果分析', '/api/v1/ai/analyze-exec', { host_id: H, command: 'df -h', output: 'ok', exit_code: 0 }],
  ['连通性测试', '/api/v1/ai/test', {}],
]) {
  const r = await call('POST', path, { body, timeoutMs: 40000 })
  const graceful = [200, 400, 409, 501, 503].includes(r.status)
  if (graceful) note(`AI · ${name} 降级行为`, `状态 ${r.status} ${r.text.slice(0, 90)}`)
  else fail(`AI · ${name} 未优雅降级`, `状态 ${r.status} ${r.text.slice(0, 110)}`)
}
check('AI · 运维报告列表', await call('GET', '/api/v1/ai/ops-reports'))

report()
