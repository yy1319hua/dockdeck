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
      <span v-if="!live" class="chart__now">{{ nowText }}</span>
    </figcaption>
    <div v-if="ready" class="chart__wrap">
      <div ref="host" class="chart__canvas" :style="{ height: height + 'px' }" />
      <!-- 触摸读值浮层：手指滑到哪就显示那一刻的值（仅手机态 + interactive） -->
      <div v-if="live" class="chart__live" aria-hidden="true">
        <span class="chart__live-time">{{ live.time }}</span>
        <span v-for="row in live.rows" :key="row.label" class="chart__live-row">
          <i class="chart__dot" :style="{ background: row.stroke }" />{{ row.label }} {{ row.text }}
        </span>
      </div>
    </div>
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
  defineProps<{
    points: HostPointWire[]
    metric: ChartMetric
    height?: number
    interactive?: boolean
  }>(),
  { height: 120, interactive: false },
)

const TITLES: Record<ChartMetric, string> = {
  cpu: 'CPU 使用率',
  mem: '内存占用',
  disk: '磁盘占用',
  net: '网络速率',
}

const host = ref<HTMLDivElement | null>(null)
// 手机态判定：图内滑轨与触摸读值只在 <768px 生效（用户 2026-10-19 需求；桌面零变化）。
const isMobile = ref(false)
let mqChart: MediaQueryList | null = null
function onMQChart(e: MediaQueryList | MediaQueryListEvent) {
  const was = isMobile.value
  isMobile.value = e.matches
  if (was !== isMobile.value) draw()
}

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

// ---- 触摸读值 ----
// uPlot 自带 cursor 只绑鼠标事件，触摸不会触发；这里手动把触摸坐标喂给 setCursor，
// 再按 cursor.idx 取该时刻各系列的值渲染成浮层（视觉对齐用户给的参考图）。
const liveIdx = ref<number | null>(null)

function fmtValue(metric: ChartMetric, v: number | null | undefined): string {
  if (v === null || v === undefined || Number.isNaN(v)) {
    return '—'
  }
  switch (metric) {
    case 'cpu':
      return formatPercent(v)
    case 'mem':
    case 'disk':
      return formatBytes(v)
    case 'net':
      return formatRate(v)
  }
  return '—'
}

const live = computed(() => {
  const idx = liveIdx.value
  if (idx === null || idx < 0 || idx >= props.points.length) {
    return null
  }
  const p = props.points[idx]
  const d = new Date(p.ts)
  const hh = String(d.getHours()).padStart(2, '0')
  const mm = String(d.getMinutes()).padStart(2, '0')
  return {
    time: `${hh}:${mm}`,
    rows: buildSeries().map((s) => ({
      label: s.label,
      stroke: s.stroke,
      text: fmtValue(props.metric, s.values[idx]),
    })),
  }
})

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
      // 触摸读值：cursor 画竖线 + 数据点；legend 关掉，改用自定义浮层
      // （uPlot 原生 legend 在图表下方、横向排列，与参考图的右上角样式不一致）。
      cursor: {
        show: props.interactive && isMobile.value,
        y: false,
        points: { size: 10, width: 2 },
      },
      legend: { show: false },
      hooks: {
        setCursor: [
          (u: uPlot) => {
            liveIdx.value = props.interactive && isMobile.value ? (u.cursor.idx ?? null) : null
          },
        ],
      },
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
  bindTouch(props.interactive && isMobile.value)
}

// uPlot 的 cursor 只认鼠标；手机端要把 touch 坐标手动喂给 setCursor。
// passive 监听，不阻断页面滚动（图表已全宽，无需横滑）。
function onTouch(e: TouchEvent) {
  if (!chart) {
    return
  }
  const t = e.touches[0]
  if (!t) {
    return
  }
  const rect = chart.over.getBoundingClientRect()
  chart.setCursor({ left: t.clientX - rect.left, top: t.clientY - rect.top })
}

function bindTouch(on: boolean) {
  const over = chart?.over
  if (!over) {
    return
  }
  over.removeEventListener('touchstart', onTouch)
  over.removeEventListener('touchmove', onTouch)
  if (on) {
    over.addEventListener('touchstart', onTouch, { passive: true })
    over.addEventListener('touchmove', onTouch, { passive: true })
  }
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

// v0.2.2 #9：flush 'post'——首次数据到达时 ready 才变 true，v-if 的 DOM 要等
// 渲染后才存在；默认 'pre' 会在 ref 仍为 null 时 draw 空转，之后无人再触发，
// 图表首屏就丢了。'post' 保证本次更新里 DOM 挂好后才画。
watch(() => [props.points, props.metric], draw, { flush: 'post' })

onMounted(() => {
  mqChart = window.matchMedia('(max-width: 767px)')
  onMQChart(mqChart)
  mqChart.addEventListener('change', onMQChart)
})

onBeforeUnmount(() => {
  mqChart?.removeEventListener('change', onMQChart)
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
  /* 手机态：纵向手势留给页面滚动，横向手势交给图表读值——
     不加这条，手指一滑先滚页面，光标不跟手（用户 2026-10-19 反馈的核心症状）。 */
  touch-action: pan-y;
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
.chart__wrap {
  position: relative;
  width: 100%;
}

/* 读数浮层：右上角，时间在上、各系列值在下（对齐用户参考图）。 */
.chart__live {
  position: absolute;
  top: 0;
  right: var(--space-1);
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  gap: 2px;
  pointer-events: none;
  padding: 2px 4px;
  border-radius: var(--radius-sm);
  background: color-mix(in srgb, var(--bg-elev-1) 78%, transparent);
  font-size: var(--text-xs);
  font-variant-numeric: tabular-nums;
  line-height: 1.35;
}

.chart__live-time {
  color: var(--fg-subtle);
}

.chart__live-row {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  color: var(--fg-default);
}

.chart__dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  flex: none;
}
</style>
