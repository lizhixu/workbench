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
//
// 观察窗口（10 分钟）结束而后台任务仍在跑时返回 in_progress，不是失败：
// 调用方必须据此提示「仍在签发中」，绝不能显示「签发失败」并诱导重试，
// 否则会重复创建 pending 占位与 CA 订单（AGENTS.md §3.7）。
export interface IssueOutcome {
  ok: boolean
  state: 'success' | 'failed' | 'in_progress'
  certId?: string
  error?: string
}

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

  async function pollTask(taskId: string): Promise<IssueTask | 'timeout' | null> {
    // 自动 DNS-01 等待窗口与后端权威 NS 传播校验（最长 10 分钟）对齐；
    // 10 分钟（300 * 2s）无终态返回 'timeout'：只是观察窗口结束，后台
    // 仍在执行（多域名订单的 CA 逐授权轮询还可能更久），不是签发失败，
    // 调用方须把“仍在进行中”和真实失败区分开。日志弹窗仍可回看。
    for (let i = 0; i < 300; i++) {
      await new Promise((r) => setTimeout(r, 2000))
      if (disposed) return null
      try {
        const task = await getIssueTask(taskId)
        if (task.status !== 'running') return task
      } catch {
        // 网络抖动：继续下一轮
      }
    }
    return 'timeout'
  }

  // 把域名批量签发为一张证书。日志弹窗复用证书中心的任务日志（实时轮询）。
  async function issueDomains(domains: string[]): Promise<IssueOutcome> {
    if (domains.length === 0) return { ok: true, state: 'success' }
    if (!canIssue.value) {
      return { ok: false, state: 'failed', error: '当前账号无证书签发权限，请联系管理员签发覆盖证书后重试' }
    }
    if (accounts.value.length === 0) {
      return { ok: false, state: 'failed', error: '尚未配置 ACME 账户，无法自动签发证书' }
    }
    let taskId = ''
    try {
      const res = await issueCert(domains, accountId.value || undefined, challenge.value || undefined)
      if (!res.ok || !res.task_id) {
        return { ok: false, state: 'failed', error: res.message || '证书签发受理失败' }
      }
      taskId = res.task_id
    } catch (e: any) {
      return { ok: false, state: 'failed', error: e?.message || '证书签发受理失败' }
    }
    logTaskId.value = taskId
    logDomains.value = domains
    logShow.value = true
    const task = await pollTask(taskId)
    if (task === 'timeout' || task === null) {
      // timeout：观察窗口结束但后台任务仍在执行；null：组件已卸载，同样不能断言失败。
      return {
        ok: false,
        state: 'in_progress',
        error: '证书签发仍在进行中（已等待 10 分钟，后台继续执行），可到证书中心查看日志；签发完成后请重新绑定',
      }
    }
    if (task.status === 'success') return { ok: true, state: 'success', certId: task.cert_id }
    return { ok: false, state: 'failed', error: task.error || '证书签发未完成，可到证书中心查看日志' }
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
