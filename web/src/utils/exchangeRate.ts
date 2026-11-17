// 汇率换算：把各币种金额换算为 CNY（基准货币）。
//
// 实时汇率走 open.er-api.com（免费、免 key、允许跨域），失败时回落内置参考价，
// 页面不因汇率接口不可用而空白。面板本身不发任何请求，汇率只在浏览器里拉取。
export interface CnyRates {
  /** 每 1 单位外币兑 CNY，如 USD: 7.1；CNY 本身恒为 1 */
  rates: Record<string, number>
  /** 汇率日期，如 '2026-11-17'；兜底时为 '内置参考价' */
  dateLabel: string
  /** true = 在线实时汇率，false = 内置参考价 */
  live: boolean
}

// 内置参考价（每 1 单位外币兑 CNY）：仅当实时接口不可用时使用，
// 数值会随时间漂移，页面会明确标注「内置参考价」。
export const FALLBACK_RATES_CNY: Record<string, number> = {
  USD: 7.1,
  EUR: 7.7,
  HKD: 0.91,
  JPY: 0.047,
  GBP: 9.2,
  USDT: 7.1,
}

export const FALLBACK_RATES: CnyRates = {
  rates: { CNY: 1, ...FALLBACK_RATES_CNY },
  dateLabel: '内置参考价',
  live: false,
}

const API_URL = 'https://open.er-api.com/v6/latest/CNY'

let cached: Promise<CnyRates> | null = null

/** 取 CNY 汇率（模块级缓存，同一页签只请求一次）。 */
export function getCnyRates(): Promise<CnyRates> {
  if (!cached) cached = fetchLiveRates()
  return cached
}

async function fetchLiveRates(): Promise<CnyRates> {
  try {
    const ctrl = new AbortController()
    const timer = setTimeout(() => ctrl.abort(), 8000)
    try {
      const resp = await fetch(API_URL, { signal: ctrl.signal })
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
      const data = await resp.json()
      if (data?.result !== 'success' || !data?.rates) throw new Error('bad response')
      // 接口返回的是「1 CNY = X 外币」，换算为「1 外币 = 1/X CNY」。
      const rates: Record<string, number> = { CNY: 1 }
      for (const [cur, perCny] of Object.entries(data.rates as Record<string, unknown>)) {
        const v = Number(perCny)
        if (v > 0) rates[cur] = 1 / v
      }
      const d = new Date(data.time_last_update_utc || Date.now())
      const dateLabel = isNaN(d.getTime())
        ? ''
        : `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}-${String(d.getUTCDate()).padStart(2, '0')}`
      return { rates, dateLabel, live: true }
    } finally {
      clearTimeout(timer)
    }
  } catch {
    return FALLBACK_RATES
  }
}

/** 把金额按币种换算为 CNY；未知币种返回 null（调用方计入「未计入」）。 */
export function toCny(amount: number, currency: string | undefined, rates: CnyRates): number | null {
  const cur = (currency || 'CNY').toUpperCase()
  const r = rates.rates[cur]
  if (r === undefined) return null
  return amount * r
}

const CURRENCY_SYMBOL: Record<string, string> = {
  CNY: '¥',
  USD: '$',
  EUR: '€',
  HKD: 'HK$',
  JPY: '¥',
  GBP: '£',
  USDT: 'USDT ',
}

/** 按币种格式化原币金额，如 USD 120 → `$120`。 */
export function formatMoney(amount: number, currency?: string): string {
  const cur = (currency || 'CNY').toUpperCase()
  const sym = CURRENCY_SYMBOL[cur] || `${cur} `
  return `${sym}${amount.toLocaleString('zh-CN', { minimumFractionDigits: 0, maximumFractionDigits: 2 })}`
}

/** 格式化 CNY 合计（千分位，最多 2 位小数）。 */
export function formatCny(amount: number): string {
  return amount.toLocaleString('zh-CN', { minimumFractionDigits: 0, maximumFractionDigits: 2 })
}
