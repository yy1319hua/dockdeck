<!--
  S7.2 单服务器详情：实时指标 + 可切换区间的历史曲线 + 容器占用。

  数据边界（诚实标注，不编造）：
   * 进程只有**总数**（HostMetrics.processes）——后端没有进程列表端点，
     所以这里显示总数并注明「明细待后端立项」，不画一张假的进程表。
   * 网络曲线画的是**速率**（相邻点差分），首点无前值可比较，留空断线而非画成 0。
   * 容器表只显示占用最高的 10 个，其余用「还有 N 个」交代，避免长列表淹没重点。
   * 实时快照与曲线每 30s 静默轮询（REFRESH_MS 与总览页、后端采集周期对齐），
     轮询失败保留上次成功的数据、只把原因记进 error——不清空页面。
-->
<template>
  <div class="host">
    <header class="host__head">
      <RouterLink class="host__back" to="/">← 总览</RouterLink>
      <div class="host__id">
        <span class="host__dot" :class="h.online ? 'host__dot--on' : 'host__dot--off'" />
        <h1 class="host__name">{{ h.server?.name ?? `服务器 #${id}` }}</h1>
      </div>
      <span class="host__seen">{{ seenText }}</span>
      <span v-if="h.refreshing" class="host__poll">刷新中…</span>
    </header>

    <p v-if="h.error" class="host__err">{{ h.error }}</p>

    <section class="host__now">
      <div class="stat">
        <span class="stat__k">CPU</span>
        <span class="stat__v">{{ cpuText }}</span>
        <span class="stat__sub">{{ coresText }}</span>
      </div>
      <div class="stat">
        <span class="stat__k">内存</span>
        <span class="stat__v">{{ memText }}</span>
        <span class="stat__sub">{{ memPctText }}</span>
      </div>
      <div class="stat">
        <span class="stat__k">{{ diskLabel }}</span>
        <span class="stat__v">{{ diskText }}</span>
        <span class="stat__sub">共 {{ diskTotalText }}</span>
      </div>
      <div class="stat">
        <span class="stat__k">负载</span>
        <span class="stat__v">{{ loadText }}</span>
        <span class="stat__sub">1 分钟</span>
      </div>
      <div class="stat">
        <span class="stat__k">进程</span>
        <span class="stat__v">{{ procText }}</span>
        <span class="stat__sub">明细待后端立项</span>
      </div>
    </section>

    <section class="host__charts">
      <div class="host__ranges">
        <span class="host__ranges-k">区间</span>
        <button
          v-for="r in RANGES"
          :key="r.key"
          type="button"
          class="btn range"
          :class="{ 'range--on': h.range === r.key }"
          :disabled="h.loading"
          @click="h.setRange(id, r.key)"
        >
          {{ r.label }}
        </button>
      </div>

      <div class="host__grid">
        <MetricChart :points="h.points" metric="cpu" interactive />
        <MetricChart :points="h.points" metric="mem" interactive />
        <MetricChart :points="h.points" metric="disk" interactive />
        <MetricChart :points="h.points" metric="net" interactive />
      </div>
    </section>

    <section class="host__ctn">
      <h2 class="host__sec-title">容器占用</h2>

      <p v-if="h.containers.length === 0" class="host__empty">
        {{ h.metrics ? '暂无容器指标（该机可能未运行 Docker）' : '暂无数据' }}
      </p>

      <table v-else class="ctn">
        <thead>
          <tr>
            <th>容器</th>
            <th class="num">CPU</th>
            <th class="num">内存</th>
            <th class="num">进程</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="c in h.containersTop" :key="c.id">
            <td class="ctn__name" :title="c.name">{{ c.name }}</td>
            <td class="num">{{ formatPercent(c.cpu_percent) }}</td>
            <td class="num">{{ formatBytes(c.mem_usage) }}<span class="ctn__dim"> / {{ formatPercent(c.mem_percent) }}</span></td>
            <td class="num">{{ c.pids }}</td>
          </tr>
        </tbody>
      </table>

      <p v-if="h.containersHidden > 0" class="host__more">
        仅显示占用最高的 {{ h.containersTop.length }} 个，还有 {{ h.containersHidden }} 个未列出
      </p>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, watch } from 'vue'
import { useRoute } from 'vue-router'
import MetricChart from '@/components/MetricChart.vue'
import { RANGES } from '@/api/metrics'
import { useHostStore } from '@/stores/host'
import { formatAgo, formatBytes, formatPercent } from '@/utils/format'

const route = useRoute()
const h = useHostStore()

const id = computed(() => {
  const raw = route.params.id
  const n = typeof raw === 'string' ? Number(raw) : Number(raw?.[0])
  return Number.isFinite(n) ? n : 0
})

onMounted(() => {
  if (id.value > 0) {
    h.start(id.value)
  }
})

onUnmounted(() => h.stop())

// 同一路由组件在服务器间切换（/servers/1 → /servers/2）不会重新挂载，必须 watch。
watch(id, (v) => {
  h.stop()
  h.reset()
  if (v > 0) {
    h.start(v)
  }
})

const m = computed(() => h.metrics)
const seenText = computed(() => formatAgo(h.server?.lastSeen))

const cpuText = computed(() => formatPercent(m.value?.cpu.usage_percent))
const coresText = computed(() => {
  const c = m.value?.cpu.cores
  return c ? `${c} 核` : '—'
})

const memText = computed(() => {
  const mem = m.value?.memory
  if (!mem || mem.total === 0) {
    return '—'
  }
  return `${formatBytes(mem.used)} / ${formatBytes(mem.total)}`
})
const memPctText = computed(() => {
  const mem = m.value?.memory
  if (!mem || mem.total === 0) {
    return '—'
  }
  return formatPercent((mem.used / mem.total) * 100)
})

const disk = computed(() => {
  const disks = m.value?.disks ?? []
  if (disks.length === 0) {
    return null
  }
  return disks.find((d) => d.mount === '/') ?? disks[0]
})
const diskLabel = computed(() => `磁盘 ${disk.value?.mount ?? ''}`.trim())
const diskText = computed(() => formatPercent(disk.value?.used_percent))
const diskTotalText = computed(() => (disk.value ? formatBytes(disk.value.total) : '—'))

const loadText = computed(() => {
  const l = m.value?.load
  return l ? l.one.toFixed(2) : '—'
})

const procText = computed(() => {
  const p = m.value?.processes
  return p === undefined || p === null ? '—' : String(p)
})
</script>

<style scoped>
.host {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.host__head {
  display: flex;
  align-items: center;
  gap: var(--space-3);
}

.host__back {
  flex: none;
  font-size: var(--text-sm);
}

.host__id {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  min-width: 0;
}

.host__dot {
  flex: none;
  width: 9px;
  height: 9px;
  border-radius: var(--radius-full);
}

.host__dot--on {
  background: var(--success);
  box-shadow: 0 0 0 3px var(--success-subtle);
}

.host__dot--off {
  background: var(--danger);
  box-shadow: 0 0 0 3px var(--danger-subtle);
}

.host__name {
  font-size: var(--text-xl);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.host__seen {
  margin-left: auto;
  flex: none;
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.host__poll {
  flex: none;
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.host__err {
  padding: var(--space-3);
  background: var(--danger-subtle);
  border: 1px solid var(--danger-border);
  border-radius: var(--radius-md);
  color: var(--danger);
  font-size: var(--text-sm);
}

.host__now {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
  gap: var(--space-3);
}

.stat {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  padding: var(--space-4);
  background: var(--bg-elev-1);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-lg);
}

.stat__k {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.stat__v {
  font-size: var(--text-xl);
  font-variant-numeric: tabular-nums;
}

.stat__sub {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.host__charts {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.host__ranges {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-wrap: wrap;
}

.host__ranges-k {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.range {
  height: 30px;
  padding: 0 var(--space-3);
  font-size: var(--text-xs);
}

.range--on {
  background: var(--accent-subtle);
  border-color: var(--accent-border);
  color: var(--fg-default);
}

.host__grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(300px, 1fr));
  gap: var(--space-3);
}

.host__ctn {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.host__sec-title {
  font-size: var(--text-lg);
}

.ctn {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}

.ctn th {
  padding: var(--space-2);
  border-bottom: 1px solid var(--border-default);
  color: var(--fg-subtle);
  font-weight: var(--weight-normal);
  text-align: left;
}

.ctn td {
  padding: var(--space-2);
  border-bottom: 1px solid var(--border-subtle);
}

.ctn .num {
  text-align: right;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.ctn__name {
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.ctn__dim {
  color: var(--fg-subtle);
}

.host__empty,
.host__more {
  padding: var(--space-4);
  background: var(--bg-elev-1);
  border: 1px dashed var(--border-default);
  border-radius: var(--radius-md);
  color: var(--fg-subtle);
  font-size: var(--text-sm);
  text-align: center;
}

.host__more {
  border-style: solid;
  text-align: left;
  padding: var(--space-2) var(--space-3);
}

/* 手机态（<768px，用户 2026-10-19 改判）：布局保持竖摞单列不变；
   滑轨做在每张图内部——时间轴横向加宽 + 触摸读值（见 MetricChart interactive）。 */
@media (max-width: 767px) {
  .host__grid {
    grid-template-columns: 1fr;
  }
}
</style>
