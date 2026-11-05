<template>
  <div class="runtime-logs">
    <NAlert v-if="!canWrite" type="info" :show-icon="true" style="margin-bottom: 12px">
      查看运行日志需要 operator 及以上角色（与主机 Docker 页一致，viewer 只能看部署历史里的构建日志）。
    </NAlert>
    <template v-else>
      <NSpace vertical :size="10" style="margin-bottom: 12px">
        <NSpace :wrap="true" align="center">
          <span class="field-label">容器</span>
          <NSelect
            v-model:value="selected"
            :options="containerOptions"
            :loading="loadingContainers"
            :disabled="!containers.length"
            placeholder="暂无可查看的容器"
            style="width: 280px"
          />
          <NButton size="small" :loading="loadingContainers" @click="loadContainers">刷新容器</NButton>
          <span v-if="selectedContainer" class="muted">{{ selectedContainer.status || '状态未知' }}</span>
          <span v-if="fallbackUsed" class="muted">未找到带应用标签的容器，已回退到记录的容器名</span>
        </NSpace>
        <NSpace :wrap="true" align="center">
          <span class="field-label">行数</span>
          <NSelect v-model:value="tail" :options="tailOptions" style="width: 110px" />
          <span class="field-label">时间范围</span>
          <NSelect v-model:value="since" :options="sinceOptions" style="width: 120px" />
          <span class="field-label">时间戳</span>
          <NSwitch v-model:value="timestamps" size="small" />
          <span class="field-label">仅错误</span>
          <NSwitch v-model:value="errorsOnly" size="small" />
          <span class="field-label">自动刷新</span>
          <NSwitch v-model:value="autoRefresh" size="small" />
          <span class="field-label">自动跟随</span>
          <NSwitch v-model:value="follow" size="small" @update:value="onFollowToggle" />
        </NSpace>
        <NSpace :wrap="true" align="center">
          <NInput
            v-model:value="search"
            clearable
            placeholder="搜索日志内容（实时过滤）"
            style="width: 320px"
          />
          <NButton size="small" :loading="loadingLogs" @click="fetchLogs(false)">刷新日志</NButton>
          <NButton size="small" :disabled="!rawText" @click="copyLogs">复制全部</NButton>
          <NButton size="small" :disabled="!rawText" @click="downloadLogs">下载 .log</NButton>
          <NButton v-if="!follow && filteredLines.length" size="small" type="primary" @click="backToLatest">
            回到最新
          </NButton>
          <span v-if="lastUpdated" class="muted">更新于 {{ fmtDateTime(lastUpdated) }}</span>
          <span v-if="pollError" class="muted">自动刷新失败，已保留上次内容：{{ pollError }}</span>
        </NSpace>
      </NSpace>

      <NAlert v-if="containersError" type="error" :show-icon="true" style="margin-bottom: 12px">
        {{ containersError }}
      </NAlert>
      <NAlert v-else-if="!loadingContainers && !containers.length" type="warning" :show-icon="true" style="margin-bottom: 12px">
        该应用当前没有可查看的容器（容器未运行、已被删除，或主机上没有带应用标签的容器）。
      </NAlert>
      <NAlert v-if="logsError" type="error" :show-icon="true" style="margin-bottom: 12px">
        {{ logsError }}
      </NAlert>

      <div ref="logBox" class="log-box" @scroll="onScroll">
        <div v-if="loadingLogs && !rawText" class="log-empty">正在拉取日志…</div>
        <div v-else-if="!selected" class="log-empty">请先选择容器</div>
        <div v-else-if="!rawText && !logsError" class="log-empty">暂无日志输出</div>
        <div v-else-if="rawText && !filteredLines.length" class="log-empty">没有匹配当前搜索 / 筛选的日志行</div>
        <div
          v-for="(line, i) in filteredLines"
          :key="`${line.raw}-${i}`"
          class="log-line"
          :class="line.cls"
        >
          <span v-if="line.ts" class="log-ts">{{ line.ts }}</span><span>{{ line.rest }}</span>
        </div>
      </div>
      <p class="tip-hint">
        日志取自容器标准输出（docker logs），单容器应用直接显示该容器，多容器 Compose 应用按服务切换查看；
        自动刷新每 2 秒静默拉取一次，与部署日志弹窗同节奏；向上滚动查看历史时自动跟随自动暂停，回到最底部恢复。
      </p>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onActivated, onDeactivated, onUnmounted, ref, watch } from 'vue'
import { NAlert, NButton, NInput, NSelect, NSpace, NSwitch, useMessage } from 'naive-ui'
import { dockerAll, dockerOp } from '../../api/hosts'
import { copyToClipboard } from '../../utils/clipboard'
import { fmtDateTime } from '../../utils/time'
import type { AppEntity } from '../../api/apps'

const props = defineProps<{
  app: AppEntity
  active: boolean
  canWrite: boolean
}>()

const message = useMessage()

interface AppContainer {
  name: string
  status: string
  service: string
}

const containers = ref<AppContainer[]>([])
const selected = ref<string | null>(null)
const loadingContainers = ref(false)
const containersError = ref('')
const fallbackUsed = ref(false)

const tail = ref(200)
const since = ref('all')
const timestamps = ref(true)
const search = ref('')
const errorsOnly = ref(false)
const autoRefresh = ref(true)
const follow = ref(true)

const rawText = ref('')
const loadingLogs = ref(false)
const logsError = ref('')
const pollError = ref('')
const lastUpdated = ref<Date | null>(null)
const logBox = ref<HTMLElement | null>(null)

const containerOptions = computed(() =>
  containers.value.map((c) => ({
    label: c.service ? `${c.service}（${c.name}）` : c.name,
    value: c.name,
  })),
)
const selectedContainer = computed(() => containers.value.find((c) => c.name === selected.value))
const tailOptions = [100, 200, 500, 1000].map((n) => ({ label: `${n} 行`, value: n }))
const sinceOptions = [
  { label: '全部', value: 'all' },
  { label: '最近 1 小时', value: '1h' },
  { label: '最近 6 小时', value: '6h' },
  { label: '最近 24 小时', value: '24h' },
  { label: '最近 7 天', value: '168h' },
]

// docker ps --format {{json .}} 把 Labels 渲染成逗号分隔的 key=value 字符串，
// 与主机 Docker 页 watchmanAppLabel 同款兼容（对象形态同样接受）。
function parseLabels(labels: unknown): Record<string, string> {
  if (!labels) return {}
  if (typeof labels === 'object') return labels as Record<string, string>
  if (typeof labels !== 'string') return {}
  const out: Record<string, string> = {}
  for (const part of labels.split(',')) {
    const eq = part.indexOf('=')
    if (eq > 0) out[part.slice(0, eq)] = part.slice(eq + 1)
  }
  return out
}

async function loadContainers() {
  if (!props.canWrite) return
  loadingContainers.value = true
  containersError.value = ''
  try {
    // dockerAll 一次往返同时带回容器与镜像，复用主机 Docker 页的取数路径。
    const res = await dockerAll(props.app.host_id)
    const rows = (res.containers || []) as any[]
    const found: AppContainer[] = []
    for (const c of rows) {
      const labels = parseLabels(c.Labels)
      // 单容器应用带 watchman.app=<id>；Compose 服务带项目标签
      // com.docker.compose.project=watchman-<id>；组网中继容器 watchman-fwd-<id>
      // 两者皆无，天然排除在外。
      if (labels['watchman.app'] !== props.app.id && labels['com.docker.compose.project'] !== `watchman-${props.app.id}`) {
        continue
      }
      found.push({
        name: (c.Names || c.ID || '').replace(/^\//, ''),
        status: c.Status || '',
        service: labels['com.docker.compose.service'] || '',
      })
    }
    // 运行中优先，滚动替换期 -next/-prev 与正式容器并存时正式容器靠服务名排序在前。
    found.sort((a, b) => {
      const aUp = a.status.startsWith('Up') ? 0 : 1
      const bUp = b.status.startsWith('Up') ? 0 : 1
      if (aUp !== bUp) return aUp - bUp
      return (a.service || a.name).localeCompare(b.service || b.name)
    })
    fallbackUsed.value = false
    if (!found.length && props.app.container_name) {
      // 标签缺失的兜底（如部署中途）：仍给记录的容器名一次机会。
      found.push({ name: props.app.container_name, status: '', service: '' })
      fallbackUsed.value = true
    }
    containers.value = found
    if (!found.some((c) => c.name === selected.value)) {
      selected.value = found.length ? found[0].name : null
    }
  } catch (e: any) {
    const msg = e.message || '加载容器列表失败'
    containersError.value = msg.includes('offline') ? '主机 Agent 离线，无法拉取容器列表' : msg
    containers.value = []
    selected.value = null
    // 容器清单失效：此前在途的日志响应不能再回填到空列表上
    invalidateInFlight()
  } finally {
    loadingContainers.value = false
  }
}

// 日志拉取请求序号：快速切换容器/行数/时间范围时会并发多个请求，
// 旧请求后到时直接丢弃，不覆盖新选中容器的日志。
// loadingSeq 单独跟踪最后一次非静默拉取：静默轮询不碰它，
// 否则一次轮询超车会把手动刷新的 loading 永久钉住。
let logSeq = 0
let loadingSeq = 0
// 在途日志请求数：静默轮询遇到慢响应时不叠加请求，避免 Agent 侧并发拉起
// 一堆 docker logs 进程；手动刷新仍可发起并用 logSeq 作废旧响应。
let inFlightCount = 0

// 停止轮询 / 切换应用 / 容器清空时统一作废在途响应：只清 timer 会让
// 迟到的响应把旧应用的日志回填到新界面（幽灵请求）。
function invalidateInFlight() {
  logSeq++
  loadingSeq = -1
  loadingLogs.value = false
}

function localizeLogError(msg: string) {
  if (msg.includes('offline')) return '主机 Agent 离线，无法拉取日志'
  if (msg.includes('No such container')) return '容器已不存在（可能已被删除或重建），请刷新容器列表后重试'
  return msg
}

async function fetchLogs(silent: boolean) {
  if (!selected.value) {
    invalidateInFlight()
    return
  }
  if (silent && inFlightCount > 0) return
  const seq = ++logSeq
  inFlightCount++
  if (!silent) {
    loadingSeq = seq
    loadingLogs.value = true
    logsError.value = ''
  }
  try {
    // 老 Agent 忽略 args_json、恒返回 --tail 200：新面板对老 Agent 自然降级，
    // 时间戳缺失时行渲染按无时间戳处理，不报错。
    const args: Record<string, unknown> = { tail: tail.value, timestamps: timestamps.value }
    if (since.value && since.value !== 'all') args.since = since.value
    const res = await dockerOp(props.app.host_id, 'logs', selected.value, '', args)
    if (seq !== logSeq) return
    const text =
      res?.raw ?? (typeof res?.data === 'string' ? res.data : res?.data != null ? JSON.stringify(res.data, null, 2) : '')
    rawText.value = text
    logsError.value = ''
    pollError.value = ''
    lastUpdated.value = new Date()
  } catch (e: any) {
    if (seq !== logSeq) return
    const msg = e.message || '拉取日志失败'
    if (silent) {
      // 自动刷新是 best-effort：失败保留现有内容，不打断正在看的日志。
      pollError.value = localizeLogError(msg)
    } else {
      logsError.value = localizeLogError(msg)
    }
  } finally {
    inFlightCount--
    if (!silent && seq === loadingSeq) loadingLogs.value = false
  }
}

let timer: number | undefined
function stopPolling(invalidate = false) {
  if (timer !== undefined) {
    clearInterval(timer)
    timer = undefined
  }
  if (invalidate) invalidateInFlight()
}
function startPolling() {
  stopPolling()
  if (!props.active || !autoRefresh.value || !selected.value) return
  timer = window.setInterval(() => fetchLogs(true), 2000)
}

async function initOrRefresh() {
  if (!props.canWrite || !props.active) return
  const hadContainers = containers.value.length > 0
  if (!hadContainers) {
    await loadContainers()
    // 首次选出容器时 selected watcher 已拉取日志，这里不再重复拉。
  } else if (selected.value) {
    await fetchLogs(true)
  }
  startPolling()
}

watch(
  () => props.active,
  (v) => {
    if (v) initOrRefresh()
    else stopPolling(true)
  },
  { immediate: true },
)
watch(
  () => props.app.id,
  () => {
    stopPolling(true)
    containers.value = []
    selected.value = null
    rawText.value = ''
    logsError.value = ''
    containersError.value = ''
    fallbackUsed.value = false
    if (props.active) initOrRefresh()
  },
)
watch(selected, () => {
  if (selected.value) {
    fetchLogs(false)
    startPolling()
  }
})
watch([tail, since, timestamps], () => {
  if (selected.value) {
    fetchLogs(false)
    startPolling()
  }
})
watch(autoRefresh, () => startPolling())
onActivated(() => {
  if (props.active) initOrRefresh()
})
onDeactivated(() => stopPolling(true))
onUnmounted(() => stopPolling(true))

const tsRe = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))\s?(.*)$/
const errorRe = /error|fatal|panic|exception|failed/i
const warnRe = /warn/i

interface LogLine {
  raw: string
  ts: string
  rest: string
  cls: string
}

const filteredLines = computed<LogLine[]>(() => {
  const q = search.value.trim().toLowerCase()
  const lines = rawText.value.split('\n')
  // docker logs 末尾换行会切出一个空段，只去这一个，行间空行保留。
  if (lines.length && lines[lines.length - 1] === '') lines.pop()
  const out: LogLine[] = []
  for (const raw of lines) {
    const m = tsRe.exec(raw)
    const ts = m ? m[1] : ''
    const rest = m ? m[2] : raw
    if (q && !raw.toLowerCase().includes(q)) continue
    const isError = errorRe.test(rest)
    if (errorsOnly.value && !isError) continue
    out.push({ raw, ts, rest, cls: isError ? 'log-line-error' : warnRe.test(rest) ? 'log-line-warn' : '' })
  }
  return out
})

function isNearBottom(el: HTMLElement) {
  return el.scrollHeight - el.scrollTop - el.clientHeight < 40
}
function onScroll() {
  const el = logBox.value
  if (el) follow.value = isNearBottom(el)
}
async function scrollToBottom() {
  await nextTick()
  const el = logBox.value
  if (el) el.scrollTop = el.scrollHeight
}
function onFollowToggle(v: boolean) {
  if (v) scrollToBottom()
}
function backToLatest() {
  follow.value = true
  scrollToBottom()
}
watch(rawText, () => {
  // 只有新日志到达才自动跟随；搜索与仅错误筛选只改变呈现行，
  // 不应把正在看的位置拽到底部。
  if (follow.value) scrollToBottom()
})

async function copyLogs() {
  if (await copyToClipboard(rawText.value)) message.success('日志已复制')
  else message.error('复制失败')
}
function downloadLogs() {
  const name = props.app.name.replace(/[\\/:*?"<>|]/g, '-')
  const blob = new Blob([rawText.value], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `${name}-logs.log`
  // 挂在 DOM 上并延迟释放，避免 Firefox / Safari 在 blob 被读取前就失效
  document.body.appendChild(a)
  a.click()
  window.setTimeout(() => {
    a.remove()
    URL.revokeObjectURL(url)
  }, 0)
}
</script>

<style scoped>
.runtime-logs {
  height: 100%;
  display: flex;
  flex-direction: column;
}
.field-label {
  font-size: 12px;
  color: var(--text-secondary);
}
.muted {
  color: var(--text-secondary);
  font-size: 12px;
}
/* 页签内滚动容器（DESIGN.md §9.7）：父级 .runtime-logs 已 height:100%，
   这里用 flex:1 + min-height:0 吃掉剩余高度，等价于显式 height:100%，
   保证只有 .log-box 一个可滚动区域。 */
.log-box {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  background: var(--code-box-bg);
  color: var(--text-primary);
  border: 1px solid var(--border-color);
  border-radius: 4px;
  padding: 12px;
  font-size: 13px;
  line-height: 1.6;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  white-space: pre-wrap;
  word-break: break-all;
  overscroll-behavior: contain;
}
.log-line {
  white-space: pre-wrap;
  word-break: break-all;
}
.log-line-error {
  color: var(--error-color, #d03050);
}
.log-line-warn {
  color: var(--warning-color, #f59e0b);
}
.log-ts {
  color: var(--text-muted);
}
.log-empty {
  color: var(--text-secondary);
  font-size: 12px;
}
/* DESIGN.md §9.16：说明文字必须自带 12px / 次要色 / 行高，不能只挂
   全局开关用的 .tip-hint 类名却没有任何样式规则。 */
.tip-hint {
  margin: 8px 0 0;
  font-size: 12px;
  line-height: 1.6;
  color: var(--text-secondary);
}
</style>
