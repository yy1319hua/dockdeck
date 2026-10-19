<!--
  S7.1 服务器监控总览：多机在线率 + 每台一张哪吒式卡片（四项指标 + CPU 曲线）。

  刷新节奏与后端采集周期对齐（30s），进页面即拉一次、离开即停表；
  「在线」以后端 servers.last_seen 为准，不由前端猜。
-->
<template>
  <div class="ov">
    <header class="ov__head">
      <div class="ov__titles">
        <h1 class="ov__title">总览</h1>
        <p class="ov__sub">
          共 {{ ov.total }} 台 · 在线 {{ ov.onlineCount }} 台<template v-if="ov.loading"> · 刷新中…</template>
        </p>
      </div>
      <button type="button" class="btn" :disabled="ov.loading" @click="onRefresh">刷新</button>
    </header>

    <section class="ov__stats">
      <div class="stat">
        <span class="stat__k">在线率</span>
        <span class="stat__v">{{ rateText }}</span>
        <span class="stat__bar"><i :style="{ width: rateText }" /></span>
      </div>
      <div class="stat">
        <span class="stat__k">平均 CPU</span>
        <span class="stat__v">{{ formatPercent(ov.avgCPU) }}</span>
      </div>
      <div class="stat">
        <span class="stat__k">内存占用</span>
        <span class="stat__v">{{ memText }}</span>
      </div>
    </section>

    <p v-if="ov.error" class="ov__err">{{ ov.error }}</p>

    <div v-if="ov.loaded && ov.total === 0" class="ov__empty">
      <p class="ov__empty-title">还没有纳管的主机</p>
      <p class="ov__empty-hint">纳管方式见 S5.8：Hub 签发 join_secret，Agent 启动时自注册。</p>
    </div>

    <div v-else class="ov__grid">
      <ServerCard v-for="s in ov.live" :key="s.server.id" :live="s" />
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted } from 'vue'
import ServerCard from '@/components/ServerCard.vue'
import { useOverviewStore } from '@/stores/overview'
import { formatBytes, formatPercent, formatRatePercent } from '@/utils/format'
import { useUiStore } from '@/stores/ui'

const ov = useOverviewStore()
const ui = useUiStore()

onMounted(() => ov.start())
onUnmounted(() => ov.stop())

const rateText = computed(() => formatRatePercent(ov.onlineRate))
const memText = computed(() =>
  ov.memTotal > 0 ? `${formatBytes(ov.memUsed)} / ${formatBytes(ov.memTotal)}` : '—',
)

async function onRefresh() {
  ui.clearError()
  await ov.refresh()
  if (ov.error) {
    ui.notifyError(new Error(ov.error))
  }
}
</script>

<style scoped>
.ov {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.ov__head {
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
}

.ov__titles {
  min-width: 0;
}

.ov__title {
  font-size: var(--text-xl);
}

.ov__sub {
  margin-top: var(--space-1);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.ov__head .btn {
  margin-left: auto;
  flex: none;
}

.ov__stats {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
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

.stat__bar {
  display: block;
  height: 3px;
  margin-top: var(--space-2);
  background: var(--bg-inset);
  border-radius: var(--radius-full);
  overflow: hidden;
}

.stat__bar i {
  display: block;
  height: 100%;
  background: var(--success);
  transition: width var(--duration-base) var(--ease-standard);
}

.ov__err {
  padding: var(--space-3);
  background: var(--danger-subtle);
  border: 1px solid var(--danger-border);
  border-radius: var(--radius-md);
  color: var(--danger);
  font-size: var(--text-sm);
}

.ov__empty {
  padding: var(--space-7) var(--space-4);
  background: var(--bg-elev-1);
  border: 1px dashed var(--border-default);
  border-radius: var(--radius-lg);
  text-align: center;
}

.ov__empty-title {
  font-size: var(--text-lg);
}

.ov__empty-hint {
  margin-top: var(--space-2);
  color: var(--fg-subtle);
  font-size: var(--text-sm);
}

.ov__grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
  gap: var(--space-3);
}

@media (max-width: 639px) {
  .ov__stats {
    grid-template-columns: 1fr;
  }
}

/* 手机态（<768px，PLAN-MOBILE M3）：服务器卡片严格单列，
   顶部统计条同步单列——拇指纵向滚动是唯一动线。 */
@media (max-width: 767px) {
  .ov__grid {
    grid-template-columns: 1fr;
  }

  .ov__stats {
    grid-template-columns: 1fr;
  }
}
</style>
