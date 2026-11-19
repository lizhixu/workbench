import type { Mount } from '../api/types'

// One filesystem can appear under several mountpoints: bind mounts reuse the
// source device, btrfs subvolumes share one device, and every ZFS dataset
// reports its pool's capacity. Key each mount by its filesystem so aggregated
// disk totals count it once. Older agents send no device, so fall back to
// identical capacity+usage as the dedupe key.
export function mountFsKey(m: Mount): string {
  const dev = (m.device ?? '').trim()
  if ((m.fstype ?? '').toLowerCase() === 'zfs') return `zfs|${m.total}|${m.used}`
  if (dev && dev !== 'none') return `dev|${dev}`
  return `cap|${m.total}|${m.used}`
}

export function dedupeMountsByFilesystem(mounts: Mount[]): Mount[] {
  const seen = new Set<string>()
  const out: Mount[] = []
  for (const m of mounts) {
    const key = mountFsKey(m)
    if (seen.has(key)) continue
    seen.add(key)
    out.push(m)
  }
  return out
}

// Disk capacity in binary units (KiB/MiB/GiB/TiB), matching df -h. Byte rates
// and monthly traffic keep their decimal formatting.
export function formatDiskBytes(n: number, digits = 2): string {
  if (!n || isNaN(n)) return '0 B'
  if (n >= 1024 ** 4) return (n / 1024 ** 4).toFixed(digits) + ' TB'
  if (n >= 1024 ** 3) return (n / 1024 ** 3).toFixed(digits) + ' GB'
  if (n >= 1024 ** 2) return (n / 1024 ** 2).toFixed(digits) + ' MB'
  if (n >= 1024) return (n / 1024).toFixed(digits) + ' KB'
  return n.toFixed(0) + ' B'
}