<!--
  uPlot 曲线封装（S7.1 起，卡片内嵌 CPU 曲线）。

  两个必须注意的点：
   1. uPlot 画在 canvas 上，**canvas 不解析 CSS 变量**，所以色值要从 :root 现读
      （值仍然来自 tokens.css——唯一色源这条纪律没有被破坏，只是多一步取值）。
   2. 组件卸载必须 destroy，否则 canvas 与事件监听会泄漏。
-->
<template>
  <div class="chart">
    <div v-if="ready" ref="host" class="chart__canvas" :style="{ height: height + 'px' }" />
    <p v-else class="chart__empty">暂无历史</p>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'
import type { HostPointWire } from '@/api/metrics'

const props = withDefaults(defineProps<{ points: HostPointWire[]; height?: number }>(), {
  height: 56,
})

const host = ref<HTMLDivElement | null>(null)
let chart: uPlot | null = null
let ro: ResizeObserver | undefined

// 两个点以下画不出趋势，宁可显示「暂无历史」也不要一条假线。
const ready = computed(() => props.points.length >= 2)

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

function draw() {
  destroy()
  const el = host.value
  if (!el || !ready.value) {
    return
  }

  const xs = props.points.map((p) => Math.floor(Date.parse(p.ts) / 1000))
  const cpu = props.points.map((p) => p.cpu_pct)

  const stroke = cssVar('--chart-1', '#58a6ff')
  const grid = cssVar('--border-subtle', '#21262d')
  const axis = cssVar('--fg-subtle', '#6e7681')

  chart = new uPlot(
    {
      width: el.clientWidth || 240,
      height: props.height,
      padding: [2, 0, 0, 0],
      legend: { show: false },
      cursor: { show: false },
      scales: { x: { time: true }, y: { range: [0, 100] } },
      axes: [
        { stroke: axis, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid }, font: '10px sans-serif' },
        { show: false },
      ],
      series: [{}, { stroke, fill: `${stroke}22`, width: 1.5, points: { show: false } }],
    },
    [xs, cpu] as uPlot.AlignedData,
    el,
  )
}

onMounted(() => {
  draw()
  if (host.value && typeof ResizeObserver !== 'undefined') {
    // 卡片宽度随断点变化，不跟着重绘的话曲线会被裁掉或留白。
    ro = new ResizeObserver(() => {
      if (chart && host.value) {
        chart.setSize({ width: host.value.clientWidth, height: props.height })
      }
    })
    ro.observe(host.value)
  }
})

watch(() => props.points, draw, { deep: false })

onBeforeUnmount(() => {
  ro?.disconnect()
  destroy()
})
</script>

<style scoped>
.chart {
  width: 100%;
}

.chart__canvas {
  width: 100%;
}

.chart__empty {
  height: 56px;
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
