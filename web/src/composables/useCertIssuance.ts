import { computed, onBeforeUnmount, ref } from 'vue'
import {
  getCertConfig, getIssueTask, issueCert, listACMEAccounts,
  type ACMEAccount, type ChallengeMode, type IssueTask,
} from '../api/certs'
import { useAuthStore } from '../stores/auth'

// 应用域名证书的自动签发编排（创建页与详情页共用）。
//
// 宝塔式语义：域名只负责“要绑什么”，证书由系统在绑定时自动匹配；没有可用
// 证书的域名由后端以 missing_domains 一次列出，前端把它们合并成一张多 SAN
// 证书提交给证书中心，签发完成后重试绑定（空 cert_id 会自动命中新证书）。
// 证书中心签发是 adminOnly，非管理员只能拿到可操作提示。
export function useCertIssuance() {
  const auth = useAuthStore()
  const canIssue = computed(() => auth.role === 'admin')

  const accounts = ref<ACMEAccount[]>([])
  const accountId = ref<string | null>(null)
  // '' 自动 | 'http-01' | 'dns-01'；手动 DNS-01 两阶段走证书中心，不在此处
  const challenge = ref<ChallengeMode>('')
  const dnsReady = ref(true)
  const accountOptions = computed(() =>
    accounts.value.map((a) => ({
      label: a.is_default ? `${a.name}（默认）` : a.name,
      value: a.id,
    })),
  )

  const logShow = ref(false)
  const logTaskId = ref('')
  const logDomains = ref<string[]>([])

  let disposed = false
  onBeforeUnmount(() => {
    disposed = true
  })

  async function loadSupport() {
    const [list, cfg] = await Promise.all([
      listACMEAccounts().catch(() => [] as ACMEAccount[]),
      getCertConfig().catch(() => null),
    ])
    accounts.value = list
    dnsReady.value = !!cfg && cfg.enabled && cfg.base_url !== ''
    if (!accountId.value) {
      const def = list.find((a) => a.is_default) || list[0]
      accountId.value = def ? def.id : null
    }
  }

  async function pollTask(taskId: string): Promise<IssueTask | null> {
    // 自动 DNS-01 一般数十秒内完成；5 分钟无终态按超时处理，日志弹窗仍可回看。
    for (let i = 0; i < 150; i++) {
      await new Promise((r) => setTimeout(r, 2000))
      if (disposed) return null
      try {
        const task = await getIssueTask(taskId)
        if (task.status !== 'running') return task
      } catch {
        // 网络抖动：继续下一轮
      }
    }
    return null
  }

  // 把域名批量签发为一张证书。日志弹窗复用证书中心的任务日志（实时轮询）。
  async function issueDomains(domains: string[]): Promise<{ ok: boolean; certId?: string; error?: string }> {
    if (domains.length === 0) return { ok: true }
    if (!canIssue.value) {
      return { ok: false, error: '当前账号无证书签发权限，请联系管理员签发覆盖证书后重试' }
    }
    if (accounts.value.length === 0) {
      return { ok: false, error: '尚未配置 ACME 账户，无法自动签发证书' }
    }
    let taskId = ''
    try {
      const res = await issueCert(domains, accountId.value || undefined, challenge.value || undefined)
      if (!res.ok || !res.task_id) {
        return { ok: false, error: res.message || '证书签发受理失败' }
      }
      taskId = res.task_id
    } catch (e: any) {
      return { ok: false, error: e?.message || '证书签发受理失败' }
    }
    logTaskId.value = taskId
    logDomains.value = domains
    logShow.value = true
    const task = await pollTask(taskId)
    if (task?.status === 'success') return { ok: true, certId: task.cert_id }
    return { ok: false, error: task?.error || '证书签发未完成，可到证书中心查看日志' }
  }

  return {
    canIssue,
    accounts,
    accountId,
    challenge,
    dnsReady,
    accountOptions,
    loadSupport,
    logShow,
    logTaskId,
    logDomains,
    issueDomains,
  }
}
