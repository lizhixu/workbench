// Panel timezone-aware time formatting.
//
// The panel (control server) can be configured with an IANA timezone
// (server.timezone, system scope, set by an admin in 系统设置 → 系统状态).
// When set, every absolute time rendered in the web UI uses that zone so all
// operators see the same wall clock regardless of where their browser runs;
// when unset, formatting falls back to the browser's local timezone (the
// historical behavior).
//
// The active zone lives in a module-level ref fed by the settings store
// (GET /version → timezone) so components using these helpers inside
// computed/render functions re-render when the admin changes it.
import { ref } from 'vue'

const panelTimezone = ref('')

/** Set the active panel timezone ('' = browser local). */
export function setPanelTimezone(tz: string) {
  panelTimezone.value = (tz || '').trim()
}

/** Current panel timezone ('' = browser local). */
export function getPanelTimezone(): string {
  return panelTimezone.value
}

type TimeInput = string | number | Date | null | undefined

function toDate(input: TimeInput): Date | null {
  if (input === null || input === undefined || input === '') return null
  const d = input instanceof Date ? input : new Date(input)
  return Number.isNaN(d.getTime()) ? null : d
}

// One formatter per (zone, shape); Intl construction is comparatively pricey
// and table renders call these per row.
const formatterCache = new Map<string, Intl.DateTimeFormat>()

function formatter(withSeconds: boolean): Intl.DateTimeFormat {
  const tz = panelTimezone.value
  const key = `${tz}|${withSeconds ? 's' : 'm'}`
  let f = formatterCache.get(key)
  if (!f) {
    const opts: Intl.DateTimeFormatOptions = {
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      hour12: false,
    }
    if (withSeconds) opts.second = '2-digit'
    if (tz) {
      try {
        f = new Intl.DateTimeFormat('zh-CN', { ...opts, timeZone: tz })
      } catch {
        // Unknown zone (hand-edited settings): stay on browser local rather
        // than blanking every timestamp in the UI.
        f = new Intl.DateTimeFormat('zh-CN', opts)
      }
    } else {
      f = new Intl.DateTimeFormat('zh-CN', opts)
    }
    formatterCache.set(key, f)
  }
  return f
}

function parts(input: TimeInput, withSeconds: boolean): Record<string, string> | null {
  const d = toDate(input)
  if (!d) return null
  const out: Record<string, string> = {}
  for (const p of formatter(withSeconds).formatToParts(d)) {
    if (p.type !== 'literal') out[p.type] = p.value
  }
  return out
}

/** 'YYYY-MM-DD HH:mm:ss' in the panel timezone. */
export function fmtDateTime(input: TimeInput): string {
  const p = parts(input, true)
  if (!p) return input ? String(input) : '-'
  return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}:${p.second}`
}

/** 'YYYY-MM-DD HH:mm' in the panel timezone. */
export function fmtDateTimeMinute(input: TimeInput): string {
  const p = parts(input, false)
  if (!p) return input ? String(input) : '-'
  return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}`
}

/** 'YYYY-MM-DD' in the panel timezone. */
export function fmtDate(input: TimeInput): string {
  const p = parts(input, false)
  if (!p) return input ? String(input) : '-'
  return `${p.year}-${p.month}-${p.day}`
}

/** 'HH:mm:ss' in the panel timezone. */
export function fmtTime(input: TimeInput): string {
  const p = parts(input, true)
  if (!p) return input ? String(input) : '-'
  return `${p.hour}:${p.minute}:${p.second}`
}
