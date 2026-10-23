<template>
  <div v-if="open" class="dd-mask" @click.self="$emit('close')">
    <div class="dd" role="dialog" aria-modal="true">
      <header class="dd__head">
        <h2 class="dd__title">部署配置 · {{ serverName }}</h2>
        <button type="button" class="dd__x" aria-label="关闭" @click="$emit('close')">✕</button>
      </header>

      <p class="dd__tip">
        用下方命令重部署后，Agent 会沿用本机<strong>原身份</strong>——<strong>不会新增服务器</strong>。
        命令中的令牌即该机长期凭证，请妥善保管、勿外泄。
      </p>

      <div v-if="loading" class="dd__state">正在获取部署配置…</div>
      <div v-else-if="error" class="dd__state dd__state--err">{{ error }}</div>
      <template v-else>
        <div class="dd__secret">
          <span class="dd__secret-label">DD_AGENT_TOKEN</span>
          <code class="dd__secret-val">{{ revealed ? token : masked }}</code>
          <button type="button" class="btn" @click="revealed = !revealed">
            {{ revealed ? '隐藏' : '显示' }}
          </button>
          <button type="button" class="btn" @click="copy(token, '令牌')">复制</button>
        </div>

        <nav class="dd__tabs" role="tablist">
          <button
            v-for="t in TABS"
            :key="t.key"
            type="button"
            role="tab"
            class="dd__tab"
            :class="{ 'dd__tab--on': tab === t.key }"
            :aria-selected="tab === t.key"
            @click="tab = t.key"
          >
            {{ t.label }}
          </button>
        </nav>

        <div class="dd__cmd">
          <pre class="dd__pre">{{ currentCommand }}</pre>
          <button type="button" class="btn dd__copy" @click="copy(currentCommand, '命令')">复制</button>
        </div>
        <p class="dd__hint">
          {{
            tab === 'docker'
              ? 'token 走环境变量，docker inspect 能看到——生产环境请注意容器与环境的可见范围。'
              : '合并进现有 compose 后执行 docker compose up -d；token 在环境变量里，注意 .env 权限。'
          }}
        </p>
      </template>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ApiError } from '@/api/errors'
import { fetchServerDeploy } from '@/api/servers'
import { buildDeployCmd, DEFAULT_DEPLOY_OPTIONS, type DeployTab } from '@/lib/deployCmd'
import { useUiStore } from '@/stores/ui'

const props = defineProps<{ serverId: number; serverName: string; open: boolean }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const ui = useUiStore()
const token = ref('')
const loading = ref(false)
const error = ref('')
const revealed = ref(false)
const tab = ref<DeployTab>('docker')

const TABS: { key: DeployTab; label: string }[] = [
  { key: 'docker', label: 'docker run' },
  { key: 'compose', label: 'compose' },
]

const masked = computed(() => 'dck_' + '•'.repeat(24))
const hubUrl = window.location.origin

const currentCommand = computed(() => {
  if (!token.value) return ''
  return buildDeployCmd(DEFAULT_DEPLOY_OPTIONS, token.value, tab.value, hubUrl)
})

onMounted(load)

async function load(): Promise<void> {
  loading.value = true
  error.value = ''
  try {
    const d = await fetchServerDeploy(props.serverId)
    token.value = d.token
  } catch (e) {
    const err = e instanceof ApiError ? e : new ApiError(0, 'deploy_failed', '获取部署配置失败')
    error.value = err.message
    ui.notifyError(err)
  } finally {
    loading.value = false
  }
}

async function copy(text: string, label: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
    ui.notifyNotice(`已复制${label}`)
  } catch {
    ui.notifyError(new ApiError(0, 'clipboard_error', '浏览器拒绝了剪贴板访问，请手动选中复制'))
  }
}
</script>

<style scoped>
.dd-mask {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.5);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: var(--space-3);
  z-index: 50;
}

.dd {
  width: min(720px, 100%);
  max-height: 90vh;
  overflow: auto;
  background: var(--bg-elev-1);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
  padding: var(--space-4);
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.dd__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
}

.dd__title {
  margin: 0;
  font-size: var(--text-lg);
}

.dd__x {
  border: 0;
  background: none;
  color: var(--fg-subtle);
  font-size: var(--text-lg);
  cursor: pointer;
}

.dd__tip {
  margin: 0;
  color: var(--fg-subtle);
  font-size: var(--text-sm);
  line-height: 1.55;
}

.dd__state {
  padding: var(--space-3);
  color: var(--fg-subtle);
}

.dd__state--err {
  color: var(--danger, #d14343);
}

.dd__secret {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-2);
  border: 1px dashed var(--border-default);
  border-radius: var(--radius-sm);
  background: var(--bg-elev-2, var(--bg-elev-1));
  flex-wrap: wrap;
}

.dd__secret-label {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.dd__secret-val {
  flex: 1 1 auto;
  min-width: 0;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: var(--text-sm);
  word-break: break-all;
}

.dd__tabs {
  display: flex;
  gap: var(--space-1);
  border-bottom: 1px solid var(--border-default);
}

.dd__tab {
  padding: var(--space-2) var(--space-3);
  border: 0;
  border-bottom: 2px solid transparent;
  background: none;
  color: var(--fg-subtle);
  font-size: var(--text-sm);
  cursor: pointer;
}

.dd__tab--on {
  border-bottom-color: var(--accent, currentColor);
  color: var(--fg-default);
}

.dd__cmd {
  position: relative;
}

.dd__pre {
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

.dd__copy {
  margin-top: var(--space-2);
}

.dd__hint {
  margin: 0;
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

@media (max-width: 767px) {
  .dd {
    padding: var(--space-3);
  }

  .dd__secret .btn,
  .dd__copy {
    min-height: 44px;
  }
}
</style>
