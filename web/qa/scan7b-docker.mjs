// Docker 容器/镜像全流程（用不冲突的宿主端口安装 Nginx）
import { call, check, note, fail, setup, report, ctx } from './lib.mjs'

await setup()
const H = ctx.hostId
const T = 120000

// 先清掉可能残留的同名容器，保证安装从干净状态开始
const pre = await call('POST', `/api/v1/hosts/${H}/exec`, {
  body: { shell: 'bash', command: 'docker rm -f watchman-app-nginx 2>/dev/null; docker ps -a --format "{{.Names}}" | tr "\\n" " "' },
  timeoutMs: 60000,
})
note('Docker · 安装前残留容器', (pre.json?.stdout || pre.text || '').slice(0, 120))

const inst = await call('POST', `/api/v1/hosts/${H}/apps/install`, {
  body: { app_id: 'nginx', host_port: 18099 },
  timeoutMs: 300000,
})
check('应用市场 · 安装 Nginx(18099)', inst, [200, 201])
note('应用市场 · 安装返回', inst.text.slice(0, 200))

const installed = await call('GET', `/api/v1/hosts/${H}/apps`, { timeoutMs: T })
check('应用市场 · 已装列表', installed)
note('应用市场 · 已装内容', installed.text.slice(0, 220))

const ps = await call('GET', `/api/v1/hosts/${H}/docker/ps`, { timeoutMs: T })
check('Docker · 容器列表', ps)
const raw = ps.json?.data
const containers = Array.isArray(raw) ? raw : raw?.containers || []
note('Docker · 容器数量', String(containers.length))
const c0 = containers[0]
if (c0) {
  const cname = c0.Names || c0.name || c0.ID || c0.id
  note('Docker · 目标容器', String(cname))
  for (const op of ['stop', 'start', 'restart']) {
    check(`Docker · ${op}`, await call('POST', `/api/v1/hosts/${H}/docker/${op}`, { body: { container: cname }, timeoutMs: T }))
  }
  const logs = await call('POST', `/api/v1/hosts/${H}/docker/logs`, { body: { container: cname }, timeoutMs: T })
  check('Docker · 查看日志', logs)
  note('Docker · 日志片段', (logs.text || '').slice(0, 150))
} else {
  fail('Docker · 容器操作', '安装成功但 ps 里没有容器')
}

const imgs = await call('GET', `/api/v1/hosts/${H}/docker/images`, { timeoutMs: T })
check('Docker · 镜像列表', imgs)
const rawI = imgs.json?.data
const images = Array.isArray(rawI) ? rawI : rawI?.images || []
note('Docker · 镜像数量', String(images.length))

// 站点可访问性（容器真的在跑）
const probe = await call('POST', `/api/v1/hosts/${H}/exec`, {
  body: { shell: 'bash', command: 'curl -sS -o /dev/null -w "%{http_code}" http://127.0.0.1:18099/ || echo FAIL' },
  timeoutMs: 60000,
})
note('Docker · 容器内 Nginx 可访问性', (probe.json?.stdout || probe.text || '').slice(0, 80))

const un = await call('DELETE', `/api/v1/hosts/${H}/apps/${encodeURIComponent('watchman-app-nginx')}`, { timeoutMs: T })
check('应用市场 · 卸载应用', un, [200, 404])
note('应用市场 · 卸载返回', un.text.slice(0, 150))

report()
