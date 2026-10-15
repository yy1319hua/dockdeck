<!--
  设计令牌色卡预览页（开发自检页，不对外暴露入口）。
  作用：把 tokens.css 的全套变量可视化，兼作 S6.1 交付的「效果图」。
  纪律：本页同样只用变量，不写字面色值。
-->
<template>
  <div class="tokens">
    <header class="tokens__head">
      <h1 class="tokens__title">深色设计令牌</h1>
      <p class="tokens__lead">
        唯一色源 <code>src/styles/tokens.css</code>；组件只用变量，不写字面色值。
      </p>
    </header>

    <section v-for="group in groups" :key="group.title" class="group">
      <h2 class="group__title">{{ group.title }}</h2>
      <p v-if="group.desc" class="group__desc">{{ group.desc }}</p>
      <ul class="swatches">
        <li v-for="item in group.items" :key="item.name" class="swatch">
          <span class="swatch__chip" :style="{ background: item.css }" />
          <code class="swatch__name">{{ item.name }}</code>
        </li>
      </ul>
    </section>

    <section class="group">
      <h2 class="group__title">圆角</h2>
      <ul class="radii">
        <li v-for="r in radii" :key="r.name" class="radii__item">
          <span class="radii__demo" :style="r.style" />
          <code>{{ r.name }}</code>
        </li>
      </ul>
    </section>

    <section class="group">
      <h2 class="group__title">间距</h2>
      <ul class="spaces">
        <li v-for="s in spaces" :key="s.name" class="spaces__item">
          <span class="spaces__box" :style="s.style"><span class="spaces__core" /></span>
          <code>{{ s.name }}</code>
        </li>
      </ul>
    </section>

    <section class="group">
      <h2 class="group__title">字号</h2>
      <ul class="types">
        <li v-for="t in types" :key="t.name" class="types__item">
          <span :style="t.style">DockDeck 监控面板</span>
          <code>{{ t.name }}</code>
        </li>
      </ul>
    </section>
  </div>
</template>

<script setup lang="ts">
type Swatch = { name: string; css: string }
type Group = { title: string; desc?: string; items: Swatch[] }
type Scale = { name: string; style: Record<string, string> }

const groups: Group[] = [
  {
    title: '背景层级',
    desc: '自底向上五层：页面底 → 卡片 → 悬浮 → 弹出 → 内嵌井。',
    items: [
      { name: '--bg-base', css: 'var(--bg-base)' },
      { name: '--bg-elev-1', css: 'var(--bg-elev-1)' },
      { name: '--bg-elev-2', css: 'var(--bg-elev-2)' },
      { name: '--bg-elev-3', css: 'var(--bg-elev-3)' },
      { name: '--bg-inset', css: 'var(--bg-inset)' },
    ],
  },
  {
    title: '前景文字',
    items: [
      { name: '--fg-default', css: 'var(--fg-default)' },
      { name: '--fg-muted', css: 'var(--fg-muted)' },
      { name: '--fg-subtle', css: 'var(--fg-subtle)' },
      { name: '--fg-on-accent', css: 'var(--fg-on-accent)' },
    ],
  },
  {
    title: '边框',
    items: [
      { name: '--border-subtle', css: 'var(--border-subtle)' },
      { name: '--border-default', css: 'var(--border-default)' },
      { name: '--border-strong', css: 'var(--border-strong)' },
    ],
  },
  {
    title: '强调色',
    desc: '操作按钮、选中态、链接。',
    items: [
      { name: '--accent', css: 'var(--accent)' },
      { name: '--accent-hover', css: 'var(--accent-hover)' },
      { name: '--accent-active', css: 'var(--accent-active)' },
      { name: '--accent-subtle', css: 'var(--accent-subtle)' },
      { name: '--accent-border', css: 'var(--accent-border)' },
    ],
  },
  {
    title: '语义色 · 实色',
    desc: '在线取 success、告警取 warning、危险操作取 danger —— 按用途而非字面色命名。',
    items: [
      { name: '--success', css: 'var(--success)' },
      { name: '--warning', css: 'var(--warning)' },
      { name: '--danger', css: 'var(--danger)' },
      { name: '--info', css: 'var(--info)' },
    ],
  },
  {
    title: '语义色 · 浅底与边框',
    desc: '告警条、确认框、徽标用：浅底配同色边框，避免实色大面积铺陈。',
    items: [
      { name: '--success-subtle', css: 'var(--success-subtle)' },
      { name: '--warning-subtle', css: 'var(--warning-subtle)' },
      { name: '--danger-subtle', css: 'var(--danger-subtle)' },
      { name: '--info-subtle', css: 'var(--info-subtle)' },
    ],
  },
  {
    title: '图表色板',
    desc: '预留给 S7.1 的 uPlot 曲线（CPU/内存/磁盘/网络多序列），S6.1 不引用。',
    items: [
      { name: '--chart-1', css: 'var(--chart-1)' },
      { name: '--chart-2', css: 'var(--chart-2)' },
      { name: '--chart-3', css: 'var(--chart-3)' },
      { name: '--chart-4', css: 'var(--chart-4)' },
      { name: '--chart-5', css: 'var(--chart-5)' },
      { name: '--chart-6', css: 'var(--chart-6)' },
    ],
  },
]

const radii: Scale[] = [
  { name: '--radius-sm', style: { borderRadius: 'var(--radius-sm)' } },
  { name: '--radius-md', style: { borderRadius: 'var(--radius-md)' } },
  { name: '--radius-lg', style: { borderRadius: 'var(--radius-lg)' } },
  { name: '--radius-full', style: { borderRadius: 'var(--radius-full)' } },
]

const spaces: Scale[] = [
  { name: '--space-1', style: { padding: 'var(--space-1)' } },
  { name: '--space-2', style: { padding: 'var(--space-2)' } },
  { name: '--space-3', style: { padding: 'var(--space-3)' } },
  { name: '--space-4', style: { padding: 'var(--space-4)' } },
  { name: '--space-5', style: { padding: 'var(--space-5)' } },
]

const types: Scale[] = [
  { name: '--text-xs', style: { fontSize: 'var(--text-xs)' } },
  { name: '--text-sm', style: { fontSize: 'var(--text-sm)' } },
  { name: '--text-base', style: { fontSize: 'var(--text-base)' } },
  { name: '--text-lg', style: { fontSize: 'var(--text-lg)' } },
  { name: '--text-xl', style: { fontSize: 'var(--text-xl)' } },
  { name: '--text-2xl', style: { fontSize: 'var(--text-2xl)' } },
]
</script>

<style scoped>
.tokens {
  max-width: 960px;
  margin: 0 auto;
  padding: var(--space-6) var(--space-5) var(--space-8);
  background: var(--bg-base);
  color: var(--fg-default);
}

.tokens__head {
  padding-bottom: var(--space-4);
  border-bottom: 1px solid var(--border-default);
}

.tokens__title {
  font-size: var(--text-2xl);
}

.tokens__lead {
  margin-top: var(--space-2);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.tokens__lead code {
  color: var(--fg-default);
  background: var(--bg-elev-2);
  border-radius: var(--radius-sm);
  padding: 0 var(--space-1);
}

.group {
  margin-top: var(--space-6);
}

.group__title {
  font-size: var(--text-lg);
}

.group__desc {
  margin-top: var(--space-1);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.swatches {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(210px, 1fr));
  gap: var(--space-2);
  margin-top: var(--space-3);
}

.swatch {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-2);
  background: var(--bg-elev-1);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
}

.swatch__chip {
  flex: none;
  width: 28px;
  height: 28px;
  border: 1px solid var(--border-default);
  border-radius: var(--radius-sm);
}

.swatch__name {
  color: var(--fg-muted);
  overflow-wrap: anywhere;
}

.radii,
.spaces,
.types {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(160px, 1fr));
  gap: var(--space-2);
  margin-top: var(--space-3);
}

.radii__item,
.spaces__item,
.types__item {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding: var(--space-3);
  background: var(--bg-elev-1);
  border: 1px solid var(--border-subtle);
  border-radius: var(--radius-md);
  color: var(--fg-muted);
}

.radii__demo {
  height: 44px;
  background: var(--accent-subtle);
  border: 1px solid var(--accent-border);
}

.spaces__box {
  display: inline-flex;
  align-self: flex-start;
  background: var(--accent-subtle);
  border: 1px dashed var(--accent-border);
}

.spaces__core {
  width: 10px;
  height: 10px;
  background: var(--accent);
  border-radius: var(--radius-sm);
}

.types__item span:first-child {
  color: var(--fg-default);
}
</style>
