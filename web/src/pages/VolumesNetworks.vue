<!--
  S7.9 卷与网络：全局页 + 顶部服务器切换器（与容器/镜像页同构），页内两分区。

  Q1-A 单页两分区而非两个独立页：卷与网络是同族只读资源，一次服务器切换、一份
  store、一个侧栏项；排查时通常也是连着看（「这个容器挂了什么卷、在哪个网络」）。

  Q3-A 展示口径（两条，都由 Engine 的数据形态决定，不是实现偷懒）：
   * 网络——Engine 的清单里就带 Containers 映射，**零额外请求**就能列出接入容器，
     故行展开直接展示名单（名字 + IPv4/IPv6 + MAC）。
   * 卷——Engine 只给 UsageData.RefCount 计数、**不给名单**。要看具体挂载须逐个
     容器 inspect，而 ContainerSummary 没有 Mounts 字段（补它要动三层），故不做；
     表下方明示这个差异，避免用户以为是页面没做。
   * 卷的 Mountpoint（宿主机绝对路径）后端 DTO 就不含，与文件模块同口径，本页
     自然也无从展示。
   * UsageData 为 null = 「Engine 没统计」，一律显示「—」——显示成 0 会让人误判
     卷没人用、可以删。
-->
<template>
  <div class="vn">
    <header class="vn__head">
      <div>
        <h1 class="vn__title">卷与网络</h1>
        <p class="vn__sub">
          {{ rs.selected ? rs.selected.name : '未选择服务器' }} · {{ rs.volumes.length }} 个卷 ·
          {{ rs.networks.length }} 个网络<template v-if="rs.refreshing"> · 刷新中…</template>
        </p>
      </div>
      <button type="button" class="btn" :disabled="rs.loading" @click="rs.load()">刷新</button>
    </header>

    <div class="vn__bar">
      <label class="vn__host">
        服务器
        <select
          class="field__input vn__select"
          :value="rs.selectedId"
          :disabled="rs.servers.length === 0"
          @change="onSelectHost"
        >
          <option v-for="s in rs.servers" :key="s.id" :value="s.id">
            {{ s.name }}{{ isOnline(s) ? '' : s.enabled ? '（离线）' : '（已禁用）' }}
          </option>
        </select>
      </label>
      <span class="vn__hint">只读视图 · 不提供创建与删除</span>
    </div>

    <p v-if="rs.error" class="vn__err">{{ rs.error }}</p>

    <template v-else>
      <!-- 分区一：数据卷 -->
      <section class="vn__sec">
        <h2 class="vn__h2">数据卷</h2>
        <table v-if="rs.volumes.length > 0" class="vn__table">
          <thead>
            <tr>
              <th>卷名</th>
              <th>驱动</th>
              <th>作用域</th>
              <th class="num">容量</th>
              <th class="num">在用容器</th>
              <th>创建时间</th>
              <th>标签</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="v in rs.volumes" :key="v.name">
              <td class="vn__name" :title="v.name">{{ v.name }}</td>
              <td>{{ v.driver || '—' }}</td>
              <td>{{ v.scope || '—' }}</td>
              <td class="num">{{ volumeSize(v) }}</td>
              <td class="num">{{ volumeRef(v) }}</td>
              <td>{{ formatRFC3339(v.createdAt) }}</td>
              <td class="vn__labels" :title="labelText(v.labels)">{{ labelText(v.labels) }}</td>
            </tr>
          </tbody>
        </table>
        <!-- 手机态（<768px，PLAN-MOBILE M7）：卷表转卡片（与 table 同条件互显）。 -->
        <div v-if="rs.volumes.length > 0" class="vn__vcards">
          <article v-for="v in rs.volumes" :key="v.name" class="vcard">
            <header class="vcard__head">
              <span class="vcard__name" :title="v.name">{{ v.name }}</span>
              <span class="vcard__size">{{ volumeSize(v) }}</span>
            </header>
            <p class="vcard__meta">
              <span>{{ v.driver || '—' }}</span>
              <span>{{ v.scope || '—' }}</span>
              <span>在用 {{ volumeRef(v) }}</span>
            </p>
            <p v-if="labelText(v.labels)" class="vcard__labels" :title="labelText(v.labels)">{{ labelText(v.labels) }}</p>
            <p class="vcard__when">{{ formatRFC3339(v.createdAt) }}</p>
          </article>
        </div>
        <p v-if="rs.volumes.length === 0" class="vn__empty">这台机上没有数据卷。</p>
        <p class="vn__note">
          「在用容器」是 Engine 给的引用计数——它<b>不给</b>具体是哪些容器在用（要看名单须逐个容器
          inspect）。卷的宿主机挂载路径按既有口径不在此展示。
        </p>
      </section>

      <!-- 分区二：容器网络 -->
      <section class="vn__sec">
        <h2 class="vn__h2">容器网络</h2>
        <table v-if="rs.networks.length > 0" class="vn__table">
          <thead>
            <tr>
              <th>网络名</th>
              <th>ID</th>
              <th>驱动</th>
              <th>子网</th>
              <th class="num">接入容器</th>
              <th>属性</th>
              <th>创建时间</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="n in rs.networks" :key="n.id">
              <tr>
                <td class="vn__name" :title="n.name">{{ n.name }}</td>
                <td class="vn__mono">{{ shortID(n.id) }}</td>
                <td>{{ n.driver || '—' }}</td>
                <td class="vn__mono">{{ n.subnets.length > 0 ? n.subnets.join('、') : '—' }}</td>
                <td class="num">
                  <button
                    v-if="n.containers.length > 0"
                    type="button"
                    class="link"
                    @click="toggleNet(n.id)"
                  >
                    {{ n.containers.length }}（{{ expanded === n.id ? '收起' : '展开' }}）
                  </button>
                  <span v-else>0</span>
                </td>
                <td class="vn__tags">
                  <span v-if="n.internal" class="badge badge--info">内部</span>
                  <span v-if="n.ingress" class="badge badge--info">ingress</span>
                  <span v-if="n.attachable" class="badge badge--off">可接入</span>
                  <span v-if="n.ipv6" class="badge badge--off">IPv6</span>
                  <span v-if="!n.internal && !n.ingress && !n.attachable && !n.ipv6">—</span>
                </td>
                <td>{{ formatRFC3339(n.created) }}</td>
              </tr>
              <tr v-if="expanded === n.id" class="vn__detail">
                <td :colspan="7">
                  <table class="vn__sub">
                    <thead>
                      <tr>
                        <th>容器</th>
                        <th>IPv4</th>
                        <th>IPv6</th>
                        <th>MAC</th>
                      </tr>
                    </thead>
                    <tbody>
                      <tr v-for="c in n.containers" :key="c.id">
                        <td class="vn__name">{{ c.name || c.id }}</td>
                        <td class="vn__mono">{{ c.ipv4 || '—' }}</td>
                        <td class="vn__mono">{{ c.ipv6 || '—' }}</td>
                        <td class="vn__mono">{{ c.mac || '—' }}</td>
                      </tr>
                    </tbody>
                  </table>
                  <p v-if="n.gateway" class="vn__gw">网关 {{ n.gateway }}</p>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
        <!-- 手机态：网络表转卡片；展开复用同一容器明细表（横滑兜底）。 -->
        <div v-if="rs.networks.length > 0" class="vn__ncards">
          <article v-for="n in rs.networks" :key="n.id" class="ncard">
            <header class="ncard__head">
              <span class="ncard__name" :title="n.name">{{ n.name }}</span>
              <span class="ncard__driver">{{ n.driver || '—' }}</span>
            </header>
            <p class="ncard__meta">
              <span>{{ shortID(n.id) }}</span>
              <span>{{ n.subnets.length > 0 ? n.subnets.join('、') : '无子网' }}</span>
            </p>
            <p class="ncard__badges">
              <span v-if="n.internal" class="badge badge--info">内部</span>
              <span v-if="n.ingress" class="badge badge--info">ingress</span>
              <span v-if="n.attachable" class="badge badge--off">可接入</span>
              <span v-if="n.ipv6" class="badge badge--off">IPv6</span>
              <span v-if="!n.internal && !n.ingress && !n.attachable && !n.ipv6" class="ncard__nobadge">—</span>
            </p>
            <footer class="ncard__acts">
              <button v-if="n.containers.length > 0" type="button" class="link" @click="toggleNet(n.id)">
                接入 {{ n.containers.length }}（{{ expanded === n.id ? '收起' : '展开' }}）
              </button>
              <span v-else class="ncard__none">未接入容器</span>
              <span class="ncard__when">{{ formatRFC3339(n.created) }}</span>
            </footer>
            <div v-if="expanded === n.id" class="ncard__detail">
              <table class="vn__sub">
                <thead>
                  <tr>
                    <th>容器</th>
                    <th>IPv4</th>
                    <th>IPv6</th>
                    <th>MAC</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="cn in n.containers" :key="cn.name">
                    <td>{{ cn.name }}</td>
                    <td class="vn__mono">{{ cn.ipv4 || '—' }}</td>
                    <td class="vn__mono">{{ cn.ipv6 || '—' }}</td>
                    <td class="vn__mono">{{ cn.mac || '—' }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
          </article>
        </div>
        <p v-if="rs.networks.length === 0" class="vn__empty">这台机上没有容器网络。</p>
        <p class="vn__note">
          接入名单由 Engine 在清单里一并给出，故展开不额外发请求——这与卷只有计数不同，差异在数据形态。
        </p>
      </section>
    </template>
  </div>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { isOnline } from '@/api/servers'
import type { Volume } from '@/api/resources'
import { useResourcesStore } from '@/stores/resources'
import { formatBytes, formatRFC3339 } from '@/utils/format'

const rs = useResourcesStore()

onMounted(() => rs.start())
onUnmounted(() => rs.stop())

/** 当前展开的网络：易失 UI 态，留页面本地 ref（同审计页/S7.5/S7.6 纪律）。 */
const expanded = ref('')

function toggleNet(id: string): void {
  expanded.value = expanded.value === id ? '' : id
}

/**
 * 容量。UsageData 为 null（Engine 未统计）或 Size 为 -1 都显示「—」——
 * 显示成 0 字节会被读成「这是个空卷」。
 */
function volumeSize(v: Volume): string {
  if (!v.usage || v.usage.size < 0) {
    return '—'
  }
  return formatBytes(v.usage.size)
}

/** 在用容器数。null（没统计）显示「—」，0 是有意义的真值（确实没人用）。 */
function volumeRef(v: Volume): string {
  if (!v.usage) {
    return '—'
  }
  return String(v.usage.refCount)
}

function labelText(labels: Record<string, string>): string {
  const keys = Object.keys(labels ?? {})
  if (keys.length === 0) {
    return '—'
  }
  return keys
    .slice()
    .sort()
    .map((k) => `${k}=${labels[k]}`)
    .join('、')
}

/** 网络 ID 取前 12 位（与 docker 的短 ID 习惯一致），全值放 title。 */
function shortID(id: string): string {
  return id.length > 12 ? id.slice(0, 12) : id || '—'
}

function onSelectHost(e: Event): void {
  const id = Number((e.target as HTMLSelectElement).value)
  if (Number.isFinite(id)) {
    void rs.selectServer(id)
  }
}
</script>

<style scoped>
.vn {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.vn__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-3);
}

.vn__title {
  margin: 0;
  font-size: var(--text-xl);
  font-weight: 600;
  color: var(--fg-default);
}

.vn__sub {
  margin: var(--space-1) 0 0;
  font-size: var(--text-sm);
  color: var(--fg-muted);
}

.vn__bar {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  flex-wrap: wrap;
}

.vn__host {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
  color: var(--fg-muted);
}

.vn__select {
  min-width: 220px;
}

.vn__hint {
  font-size: var(--text-sm);
  color: var(--fg-muted);
}

.vn__err {
  margin: 0;
  padding: var(--space-3);
  border: 1px solid var(--danger-border);
  border-radius: var(--radius-md);
  background: var(--danger-subtle);
  color: var(--danger);
  font-size: var(--text-sm);
}

.vn__sec {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.vn__h2 {
  margin: 0;
  font-size: var(--text-base);
  font-weight: 600;
  color: var(--fg-default);
}

.vn__table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}

.vn__table th,
.vn__table td {
  padding: var(--space-2) var(--space-3);
  text-align: left;
  border-bottom: 1px solid var(--border-default);
  color: var(--fg-default);
  vertical-align: top;
}

.vn__table th {
  color: var(--fg-muted);
  font-weight: 500;
  white-space: nowrap;
}

.vn__table .num {
  text-align: right;
  font-variant-numeric: tabular-nums;
}

.vn__name {
  font-weight: 500;
  color: var(--fg-default);
  max-width: 260px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.vn__mono {
  font-family: var(--font-mono);
  font-size: var(--text-xs);
}

.vn__labels {
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--fg-muted);
}

.vn__tags {
  display: flex;
  gap: var(--space-1);
  flex-wrap: wrap;
}

.vn__empty,
.vn__note {
  margin: 0;
  font-size: var(--text-sm);
  color: var(--fg-muted);
}

.vn__note {
  line-height: 1.6;
}

.vn__detail > td {
  background: var(--bg-elev-2);
}

.vn__sub {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-xs);
}

.vn__sub th,
.vn__sub td {
  padding: var(--space-1) var(--space-2);
  text-align: left;
  color: var(--fg-default);
}

.vn__sub th {
  color: var(--fg-muted);
  font-weight: 500;
}

.vn__gw {
  margin: var(--space-1) 0 0;
  font-size: var(--text-xs);
  color: var(--fg-muted);
  font-family: var(--font-mono);
}

/* 手机态（<768px，M7）：桌面只见表格，手机只见卡片。 */
.vn__vcards,
.vn__ncards {
  display: none;
}

@media (max-width: 767px) {
  .vn__table {
    display: none;
  }

  .vn__vcards,
  .vn__ncards {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
}

.vcard,
.ncard {
  background: var(--bg-elev-1);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  padding: var(--space-3);
}

.vcard__head,
.ncard__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
  min-width: 0;
}

.vcard__name,
.ncard__name {
  font-weight: var(--weight-medium);
  font-family: var(--font-mono);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.vcard__size {
  flex: none;
  color: var(--fg-muted);
  font-size: var(--text-xs);
}

.vcard__meta,
.ncard__meta {
  margin-top: var(--space-1);
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
  color: var(--fg-muted);
  font-size: var(--text-xs);
}

.vcard__labels {
  margin-top: var(--space-1);
  color: var(--fg-subtle);
  font-size: var(--text-xs);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.vcard__when {
  margin-top: var(--space-1);
  color: var(--fg-subtle);
  font-size: var(--text-xs);
  font-variant-numeric: tabular-nums;
}

.ncard__driver {
  flex: none;
  color: var(--fg-muted);
  font-size: var(--text-xs);
}

.ncard__badges {
  margin-top: var(--space-1);
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-2);
}

.ncard__nobadge,
.ncard__none {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.ncard__acts {
  margin-top: var(--space-2);
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
  font-size: var(--text-sm);
}

.ncard__when {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
  font-variant-numeric: tabular-nums;
}

.ncard__detail {
  margin-top: var(--space-2);
  overflow-x: auto;
}
</style>
