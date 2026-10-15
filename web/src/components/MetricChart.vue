<!--
  uPlot 曲线封装（按指标出图，S7.2 起支持四种：CPU / 内存 / 磁盘 / 网络）。

  三个必须注意的点：
   1. uPlot 画在 canvas 上，**canvas 不解析 CSS 变量**，所以色值要从 :root 现读
      （值仍然来自 tokens.css——唯一色源这条纪律没有被破坏，只是多一步取值）。
   2. 四项指标量纲不同（百分比 vs 字节），因此**一图一指标**，不塞多序列双 Y 轴——
      双轴图看着像两个指标相关，实际会误导。网络是例外：上下行同量纲，画两条。
   3. 组件卸载必须 destroy，否则 canvas 与事件监听会泄漏。
-->
<template>
  <figure class="chart">
    <figcaption class="chart__cap">
      <span class="chart__title">{{ title }}</span>
      <span class="chart__now">{{ nowText }}</span>
    </figcaption>
    <div v-if="ready" ref="host" class="chart__canvas" :style="{ height: height + 'px' }" />
    <p v-else class="chart__empty">暂无历史数据</p>
  </figure>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'
import type { HostPointWire } from '@/api/metrics'
import { formatBytes, formatPercent, formatRate } from '@/utils/format'

export type ChartMetric = 'cpu' | 'mem' | 'disk' | 'net'

const props = withDefaults(
  defineProps<{ points: HostPointWire[]; metric: ChartMetric; height?: number }>(),
  { height: 120 },
)

const TITLES: Record<ChartMetric, string> = {
  cpu: 'CPU 使用率',
  mem: '内存占用',
  disk: '磁盘占用',
  net: '网络速率',
}

const host = ref<HTMLDivElement | null>(null)
let chart: uPlot | null = null
let ro: ResizeObserver | undefined

// 两个点以下画不出趋势（网络要算差分，得三点才有一条线），宁可显示「暂无历史」。
const minPoints = computed(() => (props.metric === 'net' ? 3 : 2))
const ready = computed(() => props.points.length >= minPoints.value)

const title = computed(() => TITLES[props.metric])

/** 图右上角显示当前值：曲线看趋势，数字看此刻。 */
const nowText = computed(() => {
  const last = props.points[props.points.length - 1]
  if (!last) {
    return '—'
  }
  switch (props.metric) {
    case 'cpu':
      return formatPercent(last.cpu_pct)
    case 'mem':
      return formatBytes(last.mem_bytes)
    case 'disk':
      return formatBytes(last.disk_bytes)
    case 'net': {
      const r = lastTwoRate()
      return r ? `↓${formatRate(r.rx)} ↑${formatRate(r.tx)}` : '—'
    }
  }
  return '—'
})

/** 网络两点差分求速率；点不足返回 null（由 ready 兜住，不会画空图）。 */
function lastTwoRate(): { rx: number; tx: number } | null {
  const n = props.points.length
  if (n < 2) {
    return null
  }
  const a = props.points[n - 2]
  const b = props.points[n - 1]
  const dt = (Date.parse(b.ts) - Date.parse(a.ts)) / 1000
  if (dt <= 0) {
    return null
  }
  return {
    rx: Math.max(0, b.net_rx - a.net_rx) / dt,
    tx: Math.max(0, b.net_tx - a.net_tx) / dt,
  }
}

function cssVar(name: string, fallback: string): string {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return v || fallback
}

function destroy() {
  if (chart) {
    chart.destroy()
    chart = null
  }
}

/** 按指标产出序列：网络两线（同量纲），其余单线。 */
function buildSeries(): Array<{ label: string; values: (number | null)[]; stroke: string }> {
  const pts = props.points
  const c1 = cssVar('--chart-1', '#58a6ff')
  const c2 = cssVar('--chart-2', '#3fb950')
  const c3 = cssVar('--chart-3', '#d29922')

  switch (props.metric) {
    case 'cpu':
      return [{ label: 'CPU', values: pts.map((p) => p.cpu_pct), stroke: c1 }]
    case 'mem':
      return [{ label: '内存', values: pts.map((p) => p.mem_bytes), stroke: c2 }]
    case 'disk':
      return [{ label: '磁盘', values: pts.map((p) => p.disk_bytes), stroke: c3 }]
    case 'net': {
      // 首点没有前值可比，留 null（uPlot 会断线，而不是画成 0 误导读数）。
      const rx: (number | null)[] = [null]
      const tx: (number | null)[] = [null]
      for (let i = 1; i < pts.length; i++) {
        const dt = (Date.parse(pts[i].ts) - Date.parse(pts[i - 1].ts)) / 1000
        if (dt <= 0) {
          rx.push(null)
          tx.push(null)
          continue
        }
        rx.push(Math.max(0, pts[i].net_rx - pts[i - 1].net_rx) / dt)
        tx.push(Math.max(0, pts[i].net_tx - pts[i - 1].net_tx) / dt)
      }
      return [
        { label: '下行', values: rx, stroke: c1 },
        { label: '上行', values: tx, stroke: c3 },
      ]
    }
  }
  return []
}

function draw() {
  destroy()
  const el = host.value
  if (!el || !ready.value) {
    return
  }

  const xs = props.points.map((p) => Math.floor(Date.parse(p.ts) / 1000))
  const series = buildSeries()
  if (series.length === 0) {
    return
  }

  const grid = cssVar('--border-subtle', '#21262d')
  const axis = cssVar('--fg-subtle', '#6e7681')

  // Y 轴范围：百分比固定 0–100；字节量按最大值留一点余量，避免曲线贴顶。
  // 用元组而非 uPlot 的类型别名——uPlot 未导出 Range，且这里只需要 [min,max]。
  let yRange: [number, number]
  if (props.metric === 'cpu') {
    yRange = [0, 100]
  } else {
    const all = series.flatMap((s) => s.values.filter((v): v is number => v !== null))
    const max = all.length > 0 ? Math.max(...all) : 1
    yRange = [0, max > 0 ? max * 1.15 : 1]
  }

  chart = new uPlot(
    {
      width: el.clientWidth || 320,
      height: props.height,
      padding: [4, 0, 0, 0],
      legend: { show: false },
      cursor: { show: false },
      scales: { x: { time: true }, y: { range: yRange } },
      axes: [
        { stroke: axis, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid }, font: '10px sans-serif' },
        { show: false },
      ],
      series: [
        {},
        ...series.map((s) => ({
          label: s.label,
          stroke: s.stroke,
          fill: `${s.stroke}1f`,
          width: 1.5,
          points: { show: false },
        })),
      ],
    },
    [xs, ...series.map((s) => s.values)] as uPlot.AlignedData,
    el,
  )
}

onMounted(() => {
  draw()
  if (host.value && typeof ResizeObserver !== 'undefined') {
    // 宽度随断点变化，不跟着重绘的话曲线会被裁掉或留白。
    ro = new ResizeObserver(() => {
      if (chart && host.value) {
        chart.setSize({ width: host.value.clientWidth, height: props.height })
      }
    })
    ro.observe(host.value)
  }
})

watch(() => [props.points, props.metric], draw)

onBeforeUnmount(() => {
  ro?.disconnect()
  destroy()
})
</script>

<style scoped>
.chart {
  margin: 0;
  padding: var(--space-3);
  background: var(--bg-elev-1);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
}

.chart__cap {
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
  margin-bottom: var(--space-2);
}

.chart__title {
  color: var(--fg-muted);
  font-size: var(--text-xs);
}

.chart__now {
  margin-left: auto;
  font-size: var(--text-sm);
  font-variant-numeric: tabular-nums;
}

.chart__canvas {
  width: 100%;
}

.chart__empty {
  height: 120px;
  display: flex;
  align-items: center;
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

/* uPlot 自带样式是浅色底，这里按深色令牌覆盖。 */
.chart :deep(.uplot),
.chart :deep(.u-wrap) {
  background: transparent;
}
</style>
