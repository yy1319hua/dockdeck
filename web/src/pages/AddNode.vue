<!--
  PLAN-NODE-DOCS A 线：添加节点页。

  设计口径：
   * 入口在总览页（服务器列表）的「添加节点」按钮，不进侧栏——
     纳管是低频一次性动作，占一栏不值当。
   * 平台只做 Linux（用户拍板）：不设 Windows/macOS 占位 Tab，避免
     「点得动但没内容」的假入口。
   * 选项全部对应 Agent 真实参数（--dir / --tag / --fs-roots /
     --compose-dirs / --self-container / --disk-mounts），
     不造页面自己的概念。
   * 统一 wss 反向长连接（--reverse 恒发）：Agent 主动连 Hub，NAT 后 /
     无公网 IP 均可；页面不提供正向（监听）模式入口。
   * Hub 地址取 window.location.origin：公网进就是隧道域名，内网进就是内网
     地址，**页面不硬编码任何域名**（入库文件不得出现真实域名/IP）。
   * join_secret 仅签发响应可见一次 + 有效期倒计时：到期后停止展示并提示
     重新生成——密钥是「认领后即长期令牌」，泄露等于交出该机控制权。

  三种形态各自解决的问题不同，Tab 并列而非二选一：
   ① 安装脚本：幂等、带预检与探活，重跑安全（首选）
   ② docker run：已装 Docker、不想落脚本的临时机
   ③ compose：已有自己的 compose 编排，要合并进现有栈
-->
<template>
  <div class="an">
    <header class="an__head">
      <div class="an__titles">
        <h1 class="an__title">添加节点</h1>
        <p class="an__sub">
          填好选项 → 生成一次性纳管命令 → 在目标机器上执行。密钥 15 分钟内有效，且只显示这一次。
        </p>
      </div>
      <button type="button" class="btn" @click="goBack">返回总览</button>
    </header>

    <!-- ① 安装选项 -->
    <section class="card an__zone">
      <h2 class="an__zone-title">安装选项</h2>
      <p class="an__hint">
        留空即用默认值。统一使用 wss 反向长连接：Agent 主动连接 Hub，
        NAT 后 / 无公网 IP 均可，无需绑定监听端口。
      </p>

      <div class="an__grid">
        <label class="an__field">
          <span class="an__label">安装目录</span>
          <input v-model.trim="opt.dir" class="an__input" placeholder="/opt/dockdeck" />
        </label>

        <label class="an__field">
          <span class="an__label">镜像 tag</span>
          <input v-model.trim="opt.tag" class="an__input" placeholder="latest" />
          <span class="an__tip">与 Hub 版本不必一致；latest 跟随最新发布</span>
        </label>

        <label class="an__field">
          <span class="an__label">镜像加速地址</span>
          <input v-model.trim="opt.mirror" class="an__input" placeholder="如 docker.m.daocloud.io（留空 = ghcr.io 原址）" />
          <span class="an__tip">可选。国内拉取 ghcr.io 不畅时填加速域名，会拼成 <code>加速域名/ghcr.io/…</code>；三种形态同步生效</span>
        </label>

        <label class="an__field">
          <span class="an__label">文件管理根目录</span>
          <input v-model.trim="opt.fsRoots" class="an__input" :placeholder="opt.dir || '/opt/dockdeck'" />
          <span class="an__tip">逗号分隔可多个；留空 = 安装目录（面板可写，范围越小越安全）</span>
        </label>

        <label class="an__field">
          <span class="an__label">Compose 目录</span>
          <input v-model.trim="opt.composeDirs" class="an__input" :placeholder="opt.dir || '/opt/dockdeck'" />
          <span class="an__tip">留空 = 安装目录；不配则面板 Compose 页恒为空</span>
        </label>

        <label class="an__field">
          <span class="an__label">自身容器名</span>
          <input v-model.trim="opt.selfContainer" class="an__input" placeholder="dockdeck-agent" />
          <span class="an__tip">用于保护它不被面板误删/误停</span>
        </label>

        <label class="an__field">
          <span class="an__label">磁盘挂载点</span>
          <input v-model.trim="opt.diskMounts" class="an__input" placeholder="留空 = 自动探测" />
          <span class="an__tip">逗号分隔，如 /,/data</span>
        </label>
      </div>

      <div class="an__actions">
        <button type="button" class="btn btn--primary" :disabled="gen.loading" @click="onGenerate">
          {{ gen.loading ? '签发中…' : (invite ? '重新生成密钥' : '生成纳管命令') }}
        </button>
        <span v-if="gen.error" class="an__err">{{ gen.error }}</span>
      </div>
    </section>

    <!-- ② 纳管命令 -->
    <section v-if="invite" class="card an__zone">
      <h2 class="an__zone-title">
        纳管命令
        <span class="an__ttl" :class="{ 'an__ttl--dead': expired }">
          {{ expired ? '密钥已过期' : `密钥剩余 ${countdownText}` }}
        </span>
      </h2>

      <div class="an__secret">
        <span class="an__secret-label">join_secret</span>
        <code class="an__secret-val">{{ invite.joinSecret }}</code>
        <button type="button" class="btn" :disabled="expired" @click="copy(invite.joinSecret, '密钥')">复制</button>
      </div>
      <p class="an__warn">仅此一次可见。认领后它就是该机器的长期令牌——不要贴到公开渠道。</p>

      <nav class="an__tabs" role="tablist">
        <button
          v-for="t in TABS"
          :key="t.key"
          type="button"
          role="tab"
          class="an__tab"
          :class="{ 'an__tab--on': tab === t.key }"
          :aria-selected="tab === t.key"
          @click="tab = t.key"
        >
          {{ t.label }}
        </button>
      </nav>

      <div class="an__cmd">
        <pre class="an__pre">{{ currentCommand }}</pre>
        <button type="button" class="btn an__copy" :disabled="expired" @click="copy(currentCommand, '命令')">复制</button>
      </div>

      <p class="an__hint">{{ currentHint }}</p>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ApiError } from '@/api/errors'
import { createInvite, type Invite } from '@/api/servers'
import { useUiStore } from '@/stores/ui'

type TabKey = 'script' | 'docker' | 'compose'

const TABS: { key: TabKey; label: string }[] = [
  { key: 'script', label: '安装脚本' },
  { key: 'docker', label: 'docker run' },
  { key: 'compose', label: 'compose' },
]

/** 脚本来源：公开库 raw 地址（不含任何部署机域名/IP）。 */
const RAW_BASE = 'https://raw.githubusercontent.com/yy1319hua/dockdeck/main/deploy'

const router = useRouter()
const ui = useUiStore()

const opt = reactive({
  dir: '/opt/dockdeck',
  tag: 'latest',
  mirror: '',
  fsRoots: '',
  composeDirs: '',
  selfContainer: 'dockdeck-agent',
  diskMounts: '',
})

const tab = ref<TabKey>('script')
const invite = ref<Invite | null>(null)
const remaining = ref(0)
const gen = reactive({ loading: false, error: '' })
let tick: number | undefined

/** 页面不硬编码域名：从哪进来就用哪个地址（公网隧道 / 内网 / tailnet 都成立）。 */
const hubUrl = computed(() => window.location.origin)

const activeDir = computed(() => opt.dir || '/opt/dockdeck')
const activeFsRoots = computed(() => opt.fsRoots || activeDir.value)
const activeComposeDirs = computed(() => opt.composeDirs || activeDir.value)
const activeTag = computed(() => opt.tag || 'latest')
const activeSelf = computed(() => opt.selfContainer || 'dockdeck-agent')
/** 镜像加速前缀：去掉首尾空白与末尾斜杠；留空 = 直连 ghcr.io 原址。 */
const activeMirror = computed(() => opt.mirror.trim().replace(/\/+$/, ''))
/** 完整镜像引用：配了加速域名则拼成 pull-through 形式「加速域名/ghcr.io/…:tag」，
 *  否则用 ghcr.io 原址。三种形态（脚本 / docker run / compose）共用这一个值。 */
const agentImage = computed(() =>
  activeMirror.value
    ? `${activeMirror.value}/ghcr.io/yy1319hua/dockdeck-agent:${activeTag.value}`
    : `ghcr.io/yy1319hua/dockdeck-agent:${activeTag.value}`,
)

const expired = computed(() => invite.value !== null && remaining.value <= 0)

const countdownText = computed(() => {
  const s = Math.max(0, remaining.value)
  const m = Math.floor(s / 60)
  const sec = s % 60
  return `${String(m).padStart(2, '0')}:${String(sec).padStart(2, '0')}`
})

/** 安装脚本的参数：只列真实值，避免生成一堆「等于默认值」的噪音。 */
const scriptArgs = computed(() => {
  const a: string[] = [`--hub-url ${hubUrl.value}`, '--join-secret <上方的 join_secret>']
  // 统一 wss 反向长连接：Agent 主动连 Hub，不监听端口（NAT 后可用）。
  a.push('--reverse')
  a.push(`--dir ${activeDir.value}`)
  if (activeTag.value !== 'latest') {
    a.push(`--tag ${activeTag.value}`)
  }
  if (activeMirror.value) {
    a.push(`--image ${agentImage.value}`)
  }
  a.push(`--fs-roots ${activeFsRoots.value}`)
  a.push(`--compose-dirs ${activeComposeDirs.value}`)
  if (activeSelf.value !== 'dockdeck-agent') {
    a.push(`--self-container ${activeSelf.value}`)
  }
  if (opt.diskMounts) {
    a.push(`--disk-mounts ${opt.diskMounts}`)
  }
  return a
})

const currentCommand = computed(() => {
  if (!invite.value) {
    return ''
  }
  const secret = invite.value.joinSecret
  if (tab.value === 'script') {
    // 脚本用 $SCRIPT_DIR 找同目录的 docker-compose.yml，所以两个文件必须落在同一目录。
    const args = scriptArgs.value.map((x) => x.replace('<上方的 join_secret>', secret))
    return [
      '# 两条 curl + 一条执行；脚本幂等，重跑安全',
      `curl -fsSL -o /tmp/dd-agent.sh ${RAW_BASE}/install-agent.sh`,
      `curl -fsSL -o /tmp/docker-compose.yml ${RAW_BASE}/docker-compose.yml`,
      'sudo bash /tmp/dd-agent.sh \\',
      ...args.map((x, i) => (i === args.length - 1 ? `  ${x}` : `  ${x} \\`)),
    ].join('\n')
  }
  if (tab.value === 'docker') {
    const env: string[] = [
      `-e DD_HUB_URL=${hubUrl.value}`,
      `-e DD_AGENT_TOKEN=${secret}`,
      `-e DD_AGENT_FS_ROOTS=${activeFsRoots.value}`,
      `-e DD_AGENT_COMPOSE_DIRS=${activeComposeDirs.value}`,
      `-e DD_AGENT_SELF_CONTAINER=${activeSelf.value}`,
    ]
    if (opt.diskMounts) {
      env.push(`-e DD_AGENT_DISK_MOUNTS=${opt.diskMounts}`)
    }
    // 统一 wss 反向长连接：不监听端口，Agent 主动连 Hub。
    env.push('-e DD_AGENT_REVERSE=1')
    return [
      `docker run -d --name ${activeSelf.value} --restart unless-stopped --network host \\`,
      '  -v /var/run/docker.sock:/var/run/docker.sock:ro \\',
      `  -v ${activeDir.value}:${activeDir.value} \\`,
      ...env.map((x) => `  ${x} \\`),
      `  ${agentImage.value}`,
    ].join('\n')
  }
  // compose：给可直接合并进现有栈的片段（不包顶层 version，已废弃）。
  const cmdLines = [
    '      - --docker-socket',
    '      - /var/run/docker.sock',
    '      - --self-container',
    `      - ${activeSelf.value}`,
    '      - --compose-dirs',
    `      - ${activeComposeDirs.value}`,
    '      - --fs-root',
    `      - ${activeFsRoots.value}`,
  ]
  const envLines = [
    `      DD_HUB_URL: ${hubUrl.value}`,
    `      DD_AGENT_TOKEN: ${secret}`,
    `      DD_AGENT_SELF_CONTAINER: ${activeSelf.value}`,
    `      DD_AGENT_FS_ROOTS: ${activeFsRoots.value}`,
    `      DD_AGENT_COMPOSE_DIRS: ${activeComposeDirs.value}`,
  ]
  if (opt.diskMounts) {
    envLines.push(`      DD_AGENT_DISK_MOUNTS: ${opt.diskMounts}`)
  }
  // 统一 wss 反向长连接：不监听端口，Agent 主动连 Hub。
  envLines.push('      DD_AGENT_REVERSE: "1"')
  return [
    'services:',
    `  ${activeSelf.value}:`,
    `    image: ${agentImage.value}`,
    `    container_name: ${activeSelf.value}`,
    '    network_mode: host',
    '    restart: unless-stopped',
    '    command:',
    ...cmdLines,
    '    volumes:',
    '      - /var/run/docker.sock:/var/run/docker.sock:ro',
    `      - ${activeDir.value}:${activeDir.value}`,
    '    environment:',
    ...envLines,
  ].join('\n')
})

const currentHint = computed(() => {
  if (tab.value === 'script') {
    return '脚本会做预检、拉镜像、写 .env 与令牌、启动并探活；已有 .env 不会被覆盖（重跑只拉新 tag）。'
  }
  if (tab.value === 'docker') {
    return 'token 走环境变量，docker inspect 能看到——多机生产建议用安装脚本形态（token 落 600 文件）。'
  }
  return '合并进现有 compose 后执行 docker compose up -d；token 在环境变量里，注意 .env 权限。'
})

function startTick(): void {
  stopTick()
  tick = window.setInterval(() => {
    if (remaining.value > 0) {
      remaining.value -= 1
    }
    if (remaining.value <= 0) {
      stopTick()
    }
  }, 1000)
}

function stopTick(): void {
  if (tick !== undefined) {
    window.clearInterval(tick)
    tick = undefined
  }
}

async function onGenerate(): Promise<void> {
  gen.loading = true
  gen.error = ''
  try {
    // 15 分钟：够在目标机粘贴执行，泄露窗口又足够短。
    invite.value = await createInvite(15)
    remaining.value = invite.value.expiresIn || 900
    startTick()
    ui.notifyNotice('纳管命令已生成，密钥 15 分钟内有效')
  } catch (e) {
    const err = e instanceof ApiError ? e : new ApiError(0, 'invite_failed', '签发邀请失败')
    gen.error = err.message
    ui.notifyError(err)
  } finally {
    gen.loading = false
  }
}

async function copy(text: string, label: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
    ui.notifyNotice(`已复制${label}`)
  } catch {
    ui.notifyError(new ApiError(0, 'clipboard_error', '浏览器拒绝了剪贴板访问，请手动选中文本复制'))
  }
}

function goBack(): void {
  void router.push({ name: 'home' })
}

onBeforeUnmount(stopTick)
</script>

<style scoped>
.an {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.an__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-3);
}

.an__title {
  margin: 0;
  font-size: var(--text-xl);
}

.an__sub {
  margin: var(--space-1) 0 0;
  color: var(--fg-subtle);
  font-size: var(--text-sm);
  line-height: 1.5;
}

.an__zone-title {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  margin: 0 0 var(--space-3);
  font-size: var(--text-md);
}

.an__hint {
  margin: 0 0 var(--space-3);
  color: var(--fg-subtle);
  font-size: var(--text-sm);
  line-height: 1.55;
}

.an__grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: var(--space-3);
}

.an__field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  min-width: 0;
}

.an__label {
  font-size: var(--text-sm);
  color: var(--fg-default);
}

.an__input {
  width: 100%;
  padding: var(--space-2);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-sm);
  background: var(--bg-elev-1);
  color: var(--fg-default);
  font-size: var(--text-sm);
}

.an__tip {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
  line-height: 1.45;
}

.an__actions {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin-top: var(--space-3);
  flex-wrap: wrap;
}

.an__err {
  color: var(--danger, #d14343);
  font-size: var(--text-sm);
}

.an__ttl {
  margin-left: auto;
  padding: 2px var(--space-2);
  border-radius: var(--radius-sm);
  background: var(--bg-elev-2, var(--bg-elev-1));
  color: var(--fg-subtle);
  font-size: var(--text-xs);
  font-variant-numeric: tabular-nums;
}

.an__ttl--dead {
  color: var(--danger, #d14343);
}

.an__secret {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-2);
  border: 1px dashed var(--border-default);
  border-radius: var(--radius-sm);
  background: var(--bg-elev-1);
  flex-wrap: wrap;
}

.an__secret-label {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.an__secret-val {
  flex: 1 1 auto;
  min-width: 0;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: var(--text-sm);
  word-break: break-all;
}

.an__warn {
  margin: var(--space-2) 0 0;
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.an__tabs {
  display: flex;
  gap: var(--space-1);
  margin: var(--space-4) 0 var(--space-2);
  border-bottom: 1px solid var(--border-default);
}

.an__tab {
  padding: var(--space-2) var(--space-3);
  border: 0;
  border-bottom: 2px solid transparent;
  background: none;
  color: var(--fg-subtle);
  font-size: var(--text-sm);
  cursor: pointer;
}

.an__tab--on {
  border-bottom-color: var(--accent, currentColor);
  color: var(--fg-default);
}

.an__cmd {
  position: relative;
}

.an__pre {
  margin: 0;
  padding: var(--space-3);
  border-radius: var(--radius-sm);
  background: var(--bg-elev-2, var(--bg-elev-1));
  color: var(--fg-default);
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: var(--text-xs);
  line-height: 1.6;
  overflow-x: auto;
  white-space: pre;
}

.an__copy {
  margin-top: var(--space-2);
}

/* 手机态：单列 + 大触区，命令块保持横滑（不折行，折了反而难抄） */
@media (max-width: 767px) {
  .an__grid {
    grid-template-columns: minmax(0, 1fr);
  }

  .an__head {
    flex-direction: column;
  }

  .an__head .btn {
    width: 100%;
    min-height: 44px;
  }

  .an__actions .btn,
  .an__secret .btn,
  .an__copy {
    min-height: 44px;
  }

  .an__tab {
    min-height: 44px;
    padding-inline: var(--space-2);
  }
}
</style>
