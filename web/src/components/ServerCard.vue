<!--
  哪吒式服务器卡片：一台机一张，四项指标 + 一条 CPU 曲线。

  网络的坑：后端 NetStat 是开机以来的**累计**字节，速率由 Hub 拿两次快照算。
  前端只有最新一次快照，算不出速率，所以用历史曲线的最后两点求差——
  拿不到曲线（点数不足）时退回显示累计值并注明，绝不编一个速率出来。
-->
<template>
  <article
    class="card host"
    :class="{ 'host--offline': !live.online }"
    @click="goDetail"
  >
    <header class="host__head">
      <span class="host__dot" :class="live.online ? 'host__dot--on' : 'host__dot--off'" />
      <h3 class="host__name" :title="live.server.name">
        <RouterLink :to="`/servers/${live.server.id}`" @click.stop>{{ live.server.name }}</RouterLink>
      </h3>
      <span class="host__seen">{{ seenText }}</span>
    </header>

    <div class="host__metrics">
      <div class="metric">
        <span class="metric__k">CPU</span>
        <span class="metric__v">{{ cpuText }}</span>
      </div>
      <div class="metric">
        <span class="metric__k">内存</span>
        <span class="metric__v">{{ memText }}</span>
      </div>
      <div class="metric">
        <span class="metric__k">{{ diskLabel }}</span>
        <span class="metric__v">{{ diskText }}</span>
      </div>
      <div class="metric">
        <span class="metric__k">网络</span>
        <span class="metric__v">{{ netText }}</span>
      </div>
    </div>

    <div class="host__chart">
      <MetricChart :points="points" metric="cpu" :height="chartH" />
    </div>

    <p v-if="live.error" class="host__err" :title="live.error">{{ live.error }}</p>
    <p v-else-if="!live.server.enabled" class="host__note">已禁用，不再采集</p>
  </article>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import MetricChart from '@/components/MetricChart.vue'
import { fetchOverview, type HostMetricsWire, type HostPointWire } from '@/api/metrics'
import type { ServerLive } from '@/stores/overview'
import { formatAgo, formatBytes, formatPercent, formatRate } from '@/utils/format'

const props = defineProps<{ live: ServerLive }>()
const router = useRouter()

// 曲线高度随断点：手机态 72px（拇指可视性），桌面保持 56px（M3）。
const chartH = ref(56)
let mq: MediaQueryList | null = null
function onMQ(e: MediaQueryList | MediaQueryListEvent) {
  chartH.value = e.matches ? 72 : 56
}
onMounted(() => {
  mq = window.matchMedia('(max-width: 767px)')
  onMQ(mq)
  mq.addEventListener('change', onMQ)
})
onBeforeUnmount(() => mq?.removeEventListener('change', onMQ))

// 整卡可点进详情；标题本身也是真链接，键盘用户不必依赖点击。
function goDetail() {
  void router.push(`/servers/${props.live.server.id}`)
}

const points = ref<HostPointWire[]>([])
let disposed = false

async function loadPoints() {
  try {
    const r = await fetchOverview(props.live.server.id)
    if (!disposed) {
      points.value = r.points ?? []
    }
  } catch {
    // 曲线拉不到不影响卡片主体：四项指标仍然可用。
    if (!disposed) {
      points.value = []
    }
  }
}

onMounted(loadPoints)
watch(() => props.live.server.id, loadPoints)

// v0.2.2 #10：轮询器每 30s 推来新实时指标，把点 append 到曲线并滑窗——
// 停留在总览页曲线右端持续前进，不必等重新拉 overview。字段口径与后端
// SaveMetricsSnapshot 一致（rootDisk / sumNet / cpu.usage_percent / load.one）。
watch(
  () => props.live.metrics,
  (nm) => {
    if (!nm || disposed) return
    const p = pointFromLive(nm)
    const last = points.value[points.value.length - 1]
    if (last && last.ts === p.ts) return // 与拉到的历史点重叠时不重复
    points.value = [...points.value, p].slice(-130)
  },
)

function pointFromLive(m: HostMetricsWire): HostPointWire {
  const disk = m.disks.find((d) => d.mount === '/') ?? m.disks[0]
  return {
    ts: m.collected_at,
    cpu_pct: m.cpu.usage_percent,
    busy_cores: m.cpu.busy_cores,
    busy_cores_known: true,
    mem_bytes: m.memory.used,
    mem_total: m.memory.total,
    disk_bytes: disk?.used ?? 0,
    disk_total: disk?.total ?? 0,
    net_rx: m.network.reduce((a, n) => a + n.rx_bytes, 0),
    net_tx: m.network.reduce((a, n) => a + n.tx_bytes, 0),
    load1: m.load.one,
  }
}

onBeforeUnmount(() => {
  disposed = true
})

const m = computed(() => props.live.metrics)
const seenText = computed(() => formatAgo(props.live.server.lastSeen))

const cpuText = computed(() => formatPercent(m.value?.cpu.usage_percent))

const memText = computed(() => {
  const mem = m.value?.memory
  if (!mem || mem.total === 0) {
    return '—'
  }
  return `${formatBytes(mem.used)} / ${formatBytes(mem.total)}`
})

// 多挂载点时优先取根：运维说「磁盘」默认指 /。
const disk = computed(() => {
  const disks = m.value?.disks ?? []
  if (disks.length === 0) {
    return null
  }
  return disks.find((d) => d.mount === '/') ?? disks[0]
})
const diskLabel = computed(() => `磁盘 ${disk.value?.mount ?? ''}`.trim())
const diskText = computed(() => {
  const d = disk.value
  if (!d || d.total === 0) {
    return '—'
  }
  return `${formatPercent(d.used_percent)} · ${formatBytes(d.used)}`
})

/** 用曲线最后两点求差得到速率；点数不足时退回累计值并标明是累计。 */
const netText = computed(() => {
  const pts = points.value
  if (pts.length >= 2) {
    const a = pts[pts.length - 2]
    const b = pts[pts.length - 1]
    const dt = (Date.parse(b.ts) - Date.parse(a.ts)) / 1000
    if (dt > 0) {
      const rx = Math.max(0, b.net_rx - a.net_rx) / dt
      const tx = Math.max(0, b.net_tx - a.net_tx) / dt
      return `↓${formatRate(rx)} ↑${formatRate(tx)}`
    }
  }
  const nets = m.value?.network ?? []
  if (nets.length === 0) {
    return '—'
  }
  const rx = nets.reduce((acc, n) => acc + n.rx_bytes, 0)
  const tx = nets.reduce((acc, n) => acc + n.tx_bytes, 0)
  return `累计 ↓${formatBytes(rx)} ↑${formatBytes(tx)}`
})
</script>

<style scoped>
.host {
  cursor: pointer;
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding: var(--space-4);
}

.host--offline {
  opacity: 0.62;
}

.host__head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

.host__dot {
  flex: none;
  width: 8px;
  height: 8px;
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

.host__name a {
  color: inherit;
}

.host__name a:hover {
  color: var(--accent-hover);
}

.host__name {
  font-size: var(--text-base);
  font-weight: var(--weight-semibold);
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

.host__metrics {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: var(--space-3);
}

.metric {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  min-width: 0;
}

.metric__k {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.metric__v {
  font-size: var(--text-sm);
  font-variant-numeric: tabular-nums;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.host__chart {
  margin-top: var(--space-1);
}

.host__err,
.host__note {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.host__err {
  color: var(--danger);
}
</style>
