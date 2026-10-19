<!--
  PLAN-NODE-DOCS B：API 文档页（/docs）。

  三条硬约束：
   ① 自包含零外链——不引 CDN、不引 iframe、不引 web 字体；样式与渲染全部本地，
      验收标准是「网络面板里除同源请求外一条都没有」。
   ② 数据同源——页面只 fetch `/docs/openapi.json`（后端从 route.Table 实时生成）。
      文档不是手写的第二份真相，路由一改这里立刻跟上。
   ③ 公开只读——不加 requiresAuth；页面只展示接口形态，不含任何凭据或内网地址。

  为什么不引 Scalar / Swagger UI：
   两者都要从 CDN 或额外包拉资源，且默认带字体外链，与「内网离线可用」冲突。
    本页是手写渲染：端点列表 + 参数表 + 响应 + schema 摘要，覆盖查文档的 90% 场景，
    真正要做请求调试请用 curl / Postman 导入 /docs/openapi.json。
-->
<template>
  <div class="doc">
    <header class="doc__head">
      <div class="doc__titles">
        <h1 class="doc__title">{{ title }}</h1>
        <p class="doc__sub">
          <template v-if="spec">版本 {{ spec.info.version }} · {{ items.length }} 个端点</template>
          <template v-else>正在加载接口清单…</template>
        </p>
      </div>
      <nav class="doc__links">
        <a class="doc__link" href="/docs/llms.txt" target="_blank" rel="noopener">llms.txt</a>
        <a class="doc__link" href="/docs/api.md" target="_blank" rel="noopener">api.md</a>
        <a class="doc__link" href="/docs/openapi.json" target="_blank" rel="noopener">openapi.json</a>
      </nav>
    </header>

    <p v-if="err" class="doc__err">
      文档加载失败：{{ err }}
      <button type="button" class="doc__retry" @click="load">重试</button>
    </p>

    <div v-else class="doc__body">
      <aside class="doc__side">
        <section v-for="g in groups" :key="g.tag" class="doc__group">
          <p class="doc__tag">{{ g.tag }}</p>
          <button
            v-for="e in g.items"
            :key="e.key"
            type="button"
            class="doc__ep"
            :class="{ 'doc__ep--on': e.key === activeKey }"
            @click="select(e.key)"
          >
            <span class="m" :data-m="e.method">{{ e.method }}</span>
            <span class="doc__ep-path">{{ e.path }}</span>
          </button>
        </section>
      </aside>

      <main class="doc__main">
        <article v-if="active" class="doc__article">
          <h2 class="doc__op">
            <span class="m" :data-m="active.method">{{ active.method }}</span>
            <code class="doc__op-path">{{ active.path }}</code>
          </h2>
          <p v-if="active.op.summary" class="doc__summary">{{ active.op.summary }}</p>
          <p v-if="active.op.deprecated" class="doc__deprecated">该端点已废弃，后续版本可能移除。</p>
          <p v-if="active.op.description" class="doc__desc">{{ active.op.description }}</p>

          <h3 v-if="active.op.parameters?.length" class="doc__h3">参数</h3>
          <table v-if="active.op.parameters?.length" class="doc__table">
            <thead>
              <tr><th>名称</th><th>位置</th><th>必填</th><th>类型</th><th>说明</th></tr>
            </thead>
            <tbody>
              <tr v-for="p in active.op.parameters" :key="p.in + p.name">
                <td><code>{{ p.name }}</code></td>
                <td>{{ p.in }}</td>
                <td>{{ p.required ? '是' : '否' }}</td>
                <td>{{ p.schema?.type || '—' }}</td>
                <td>{{ p.description || '—' }}</td>
              </tr>
            </tbody>
          </table>

          <h3 v-if="active.op.requestBody" class="doc__h3">请求体</h3>
          <p v-if="active.op.requestBody" class="doc__desc">
            {{ active.op.requestBody.required ? '必填' : '可选' }} ·
            {{ Object.keys(active.op.requestBody.content || {}).join(' / ') || '—' }}
          </p>

          <h3 class="doc__h3">响应</h3>
          <table class="doc__table">
            <thead>
              <tr><th>状态</th><th>说明</th><th>结构</th></tr>
            </thead>
            <tbody>
              <tr v-for="r in responses" :key="r.code">
                <td><code>{{ r.code }}</code></td>
                <td>{{ r.desc }}</td>
                <td>
                  <button
                    v-if="r.ref"
                    type="button"
                    class="doc__ref"
                    :class="{ 'doc__ref--on': openSchema === r.ref }"
                    @click="toggleSchema(r.ref)"
                  >
                    {{ r.ref }}
                  </button>
                  <span v-else>—</span>
                </td>
              </tr>
            </tbody>
          </table>

          <section v-if="openSchemaProps.length" class="doc__schema">
            <h3 class="doc__h3">{{ openSchema }} 字段</h3>
            <table class="doc__table">
              <thead>
                <tr><th>字段</th><th>类型</th><th>说明</th></tr>
              </thead>
              <tbody>
                <tr v-for="f in openSchemaProps" :key="f.name">
                  <td><code>{{ f.name }}</code></td>
                  <td>{{ f.type }}</td>
                  <td>{{ f.desc || '—' }}</td>
                </tr>
              </tbody>
            </table>
          </section>
        </article>

        <p v-else-if="!err" class="doc__empty">加载中…</p>
      </main>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

interface SchemaNode {
  type?: string
  format?: string
  description?: string
  $ref?: string
  properties?: Record<string, SchemaNode>
  items?: SchemaNode
  additionalProperties?: SchemaNode | boolean
  enum?: unknown[]
}

interface SpecParam {
  name: string
  in: string
  required?: boolean
  description?: string
  schema?: SchemaNode
}

interface SpecMedia {
  schema?: SchemaNode
}

interface SpecResponse {
  description: string
  content?: Record<string, SpecMedia>
}

interface SpecOp {
  operationId?: string
  summary?: string
  description?: string
  tags?: string[]
  parameters?: SpecParam[]
  requestBody?: { required?: boolean; content?: Record<string, SpecMedia> }
  responses?: Record<string, SpecResponse>
  deprecated?: boolean
}

interface Spec {
  info: { title: string; version: string; description?: string }
  tags?: { name: string; description?: string }[]
  paths: Record<string, Record<string, SpecOp>>
  components?: { schemas?: Record<string, SchemaNode> }
}

interface Endpoint {
  key: string
  method: string
  path: string
  tag: string
  op: SpecOp
}

/** 方法与展示顺序：读在前、写在后，与使用频率一致。 */
const METHOD_ORDER = ['get', 'post', 'put', 'patch', 'delete'] as const

const route = useRoute()
const router = useRouter()

const spec = ref<Spec | null>(null)
const items = ref<Endpoint[]>([])
const activeKey = ref('')
const err = ref('')
const openSchema = ref('')

const title = computed(() => spec.value?.info.title ?? 'DockDeck API')

const active = computed(() => items.value.find((e) => e.key === activeKey.value) ?? null)

/** 按标签分组，标签顺序沿用 spec 声明，未声明的按字典序追加。 */
const groups = computed(() => {
  const byTag = new Map<string, Endpoint[]>()
  for (const e of items.value) {
    const list = byTag.get(e.tag)
    if (list) {
      list.push(e)
    } else {
      byTag.set(e.tag, [e])
    }
  }
  const order: string[] = []
  for (const t of spec.value?.tags ?? []) {
    if (byTag.has(t.name)) {
      order.push(t.name)
    }
  }
  for (const t of byTag.keys()) {
    if (!order.includes(t)) {
      order.push(t)
    }
  }
  return order.map((tag) => ({ tag, items: byTag.get(tag) ?? [] }))
})

const responses = computed(() => {
  const op = active.value?.op
  if (!op?.responses) {
    return []
  }
  return Object.keys(op.responses)
    .sort((a, b) => {
      const na = Number(a)
      const nb = Number(b)
      if (Number.isNaN(na) || Number.isNaN(nb)) {
        return a.localeCompare(b)
      }
      return na - nb
    })
    .map((code) => {
      const r = op.responses?.[code]
      const media = r?.content?.['application/json']
      return {
        code,
        desc: r?.description ?? '',
        ref: refName(media?.schema) || refName(r?.content?.['text/plain']?.schema),
      }
    })
})

const openSchemaProps = computed(() => {
  const name = openSchema.value
  if (!name) {
    return []
  }
  return schemaProps(name)
})

/** `$ref` 只取末段名；模板里不能用 `$` 开头的键，故统一走函数。 */
function refName(s?: SchemaNode): string {
  const ref = s?.$ref
  if (!ref) {
    return ''
  }
  const i = ref.lastIndexOf('/')
  return i >= 0 ? ref.slice(i + 1) : ref
}

function schemaProps(name: string): { name: string; type: string; desc: string }[] {
  const node = spec.value?.components?.schemas?.[name]
  if (!node?.properties) {
    return []
  }
  return Object.entries(node.properties).map(([k, v]) => ({
    name: k,
    type: v?.type || refName(v?.items) || refName(v) || '—',
    desc: v?.description ?? '',
  }))
}

function toggleSchema(name: string): void {
  openSchema.value = openSchema.value === name ? '' : name
}

function select(key: string): void {
  activeKey.value = key
  openSchema.value = ''
  // 端点写进 query：可把某条接口的链接直接发给同事。
  void router.replace({ query: { op: key } })
}

function flatten(s: Spec): Endpoint[] {
  const out: Endpoint[] = []
  for (const path of Object.keys(s.paths).sort()) {
    const item = s.paths[path]
    for (const m of METHOD_ORDER) {
      const op = item[m]
      if (!op) {
        continue
      }
      out.push({
        key: `${m.toUpperCase()} ${path}`,
        method: m.toUpperCase(),
        path,
        tag: op.tags?.[0] || 'Other',
        op,
      })
    }
  }
  return out
}

async function load(): Promise<void> {
  err.value = ''
  try {
    const res = await fetch('/docs/openapi.json', { headers: { Accept: 'application/json' } })
    if (!res.ok) {
      throw new Error(`HTTP ${res.status}`)
    }
    const data = (await res.json()) as Spec
    spec.value = data
    items.value = flatten(data)

    const wanted = typeof route.query.op === 'string' ? route.query.op : ''
    activeKey.value =
      items.value.find((e) => e.key === wanted)?.key ?? items.value[0]?.key ?? ''
  } catch (e) {
    err.value = e instanceof Error ? e.message : '未知错误'
  }
}

onMounted(load)
</script>

<style scoped>
.doc {
  min-height: 100vh;
  padding: var(--space-4);
  background: var(--bg-base, #0f1115);
  color: var(--fg-default);
  font-size: var(--text-sm);
}

.doc__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-3);
  padding-bottom: var(--space-3);
  border-bottom: 1px solid var(--border-default);
  flex-wrap: wrap;
}

.doc__title {
  margin: 0;
  font-size: var(--text-xl);
}

.doc__sub {
  margin: var(--space-1) 0 0;
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.doc__links {
  display: flex;
  gap: var(--space-2);
  flex-wrap: wrap;
}

.doc__link {
  padding: var(--space-1) var(--space-2);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-sm);
  color: var(--fg-default);
  font-size: var(--text-xs);
  text-decoration: none;
}

.doc__err {
  color: var(--danger, #d14343);
  font-size: var(--text-sm);
}

.doc__retry {
  margin-left: var(--space-2);
  background: none;
  border: 0;
  color: var(--accent, #4a9eff);
  cursor: pointer;
  text-decoration: underline;
}

.doc__body {
  display: grid;
  grid-template-columns: 280px minmax(0, 1fr);
  gap: var(--space-4);
  margin-top: var(--space-3);
}

.doc__side {
  position: sticky;
  top: var(--space-4);
  align-self: start;
  max-height: calc(100vh - var(--space-6));
  overflow-y: auto;
  padding-right: var(--space-2);
}

.doc__group + .doc__group {
  margin-top: var(--space-3);
}

.doc__tag {
  margin: 0 0 var(--space-1);
  color: var(--fg-subtle);
  font-size: var(--text-xs);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}

.doc__ep {
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
  width: 100%;
  padding: 4px var(--space-1);
  border: 0;
  border-radius: var(--radius-sm);
  background: none;
  color: var(--fg-subtle);
  text-align: left;
  cursor: pointer;
  font-size: var(--text-xs);
}

.doc__ep--on {
  background: var(--bg-elev-1);
  color: var(--fg-default);
}

.doc__ep-path {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* 方法徽章：只用颜色区分类别，不引图标字体 */
.m {
  flex: none;
  min-width: 44px;
  padding: 1px 4px;
  border-radius: 3px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 10px;
  font-weight: 600;
  text-align: center;
  background: var(--bg-elev-2, #1b1f27);
}

.m[data-m='GET'] {
  color: #4ade80;
}
.m[data-m='POST'] {
  color: #60a5fa;
}
.m[data-m='PUT'] {
  color: #fbbf24;
}
.m[data-m='PATCH'] {
  color: #c084fc;
}
.m[data-m='DELETE'] {
  color: #f87171;
}

.doc__main {
  min-width: 0;
}

.doc__op {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  margin: 0 0 var(--space-2);
  font-size: var(--text-md);
  flex-wrap: wrap;
}

.doc__op-path {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: var(--text-sm);
  word-break: break-all;
}

.doc__summary {
  margin: 0 0 var(--space-2);
  font-weight: 600;
}

.doc__desc {
  margin: 0 0 var(--space-3);
  color: var(--fg-subtle);
  line-height: 1.6;
  white-space: pre-wrap;
}

.doc__deprecated {
  margin: 0 0 var(--space-2);
  color: #fbbf24;
  font-size: var(--text-xs);
}

.doc__h3 {
  margin: var(--space-4) 0 var(--space-2);
  font-size: var(--text-sm);
  color: var(--fg-default);
}

.doc__table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-xs);
}

.doc__table th,
.doc__table td {
  padding: 6px var(--space-2);
  border-bottom: 1px solid var(--border-default);
  text-align: left;
  vertical-align: top;
}

.doc__table th {
  color: var(--fg-subtle);
  font-weight: 500;
}

.doc__ref {
  padding: 1px 6px;
  border: 1px solid var(--border-default);
  border-radius: 3px;
  background: none;
  color: var(--accent, #4a9eff);
  cursor: pointer;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 11px;
}

.doc__ref--on {
  background: var(--bg-elev-2, #1b1f27);
}

.doc__empty {
  color: var(--fg-subtle);
}

/* 手机态：侧栏移到上方并横向滚动，正文全宽 */
@media (max-width: 767px) {
  .doc__body {
    grid-template-columns: minmax(0, 1fr);
  }

  .doc__side {
    position: static;
    max-height: none;
    display: flex;
    gap: var(--space-3);
    overflow-x: auto;
    padding-bottom: var(--space-2);
    border-bottom: 1px solid var(--border-default);
  }

  .doc__group {
    flex: none;
  }

  .doc__ep {
    min-height: 40px;
  }

  .doc__head .doc__link {
    min-height: 40px;
    display: inline-flex;
    align-items: center;
  }
}
</style>
