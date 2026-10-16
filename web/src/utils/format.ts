/*
 * 展示层格式化。后端一律给原始单位（byte、累计值、0–100 的百分比），
 * 换算集中在这里，免得各组件各写一套「除以 1024」然后算出三种不同的 GiB。
 */

const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'] as const

/** 字节数转人类可读。用 1024 进制并标 KiB/MiB——运维看的是 2 进制，别写成 KB。 */
export function formatBytes(n: number | undefined | null): string {
  if (!n || n <= 0) {
    return '0 B'
  }
  let v = n
  let i = 0
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024
    i++
  }
  const digits = i === 0 || v >= 100 ? 0 : 1
  return `${v.toFixed(digits)} ${UNITS[i]}`
}

export function formatPercent(v: number | undefined | null, digits = 1): string {
  if (v === undefined || v === null || Number.isNaN(v)) {
    return '—'
  }
  return `${v.toFixed(digits)}%`
}

/** 速率：字节/秒 → 如 1.2 MiB/s。 */
export function formatRate(bytesPerSec: number | undefined | null): string {
  if (!bytesPerSec || bytesPerSec <= 0) {
    return '0 B/s'
  }
  return `${formatBytes(bytesPerSec)}/s`
}

/** 相对时间：给「最后上报」用——绝对时间不如「3 分钟前」直观。 */
export function formatAgo(iso: string | undefined | null, now: number = Date.now()): string {
  if (!iso) {
    return '从未上报'
  }
  const t = Date.parse(iso)
  if (Number.isNaN(t)) {
    return '未知'
  }
  const sec = Math.max(0, Math.floor((now - t) / 1000))
  if (sec < 60) {
    return `${sec} 秒前`
  }
  const min = Math.floor(sec / 60)
  if (min < 60) {
    return `${min} 分钟前`
  }
  const hr = Math.floor(min / 60)
  if (hr < 24) {
    return `${hr} 小时前`
  }
  return `${Math.floor(hr / 24)} 天前`
}

/** 在线率：0–1 → 百分比文本。 */
export function formatRatePercent(ratio: number): string {
  if (!Number.isFinite(ratio) || ratio <= 0) {
    return '0%'
  }
  return `${Math.round(ratio * 100)}%`
}

/**
 * Unix 秒 → 本地日期时间（Engine 的 Created 口径，秒而非毫秒）。
 * 镜像/构建时间这类「过去某时刻」用绝对时间比「3 天前」更可用——要核对版本。
 */
export function formatWhen(unixSec: number | undefined | null): string {
  if (!unixSec || !Number.isFinite(unixSec) || unixSec <= 0) {
    return '—'
  }
  const d = new Date(unixSec * 1000)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}
