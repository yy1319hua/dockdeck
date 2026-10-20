<!--
  S7.8 设置页：单页六分区（方案 Q1=A——设置是低频访问的聚合页，分区比多页
  Tab 更直达；不搞路由嵌套，一个 /settings 全含）。

  分区与方案各条的落点：
   * Q1 单页多分区：账号安全（改密）/ API Token 管理 / 保护名单 / TLS 指纹 /
     整库备份 / 版本信息（2026-10-20 新增），顺序按「身份 → 凭据 → 边界 → 通道 → 兜底 → 信息」排；
   * Q2 保护名单存储格式改造 + 逐条编译校验：前端 textarea 一行一条，
     归一（trim/去空行）后整名单 PUT；坏正则 400 的「第 N 条」文案原样
     展示——它是用户改错的直接指引。保存即时生效（后端逐请求读 settings）；
   * Q3 整库 VACUUM INTO 快照下载：NoEnvelope 裸流，api 层直 fetch；
   * Q5 TLS 指纹 hex 冒号分隔 + 复制；不展示过期时间（指纹是「当下这一枚」
     的标识，过期归宿主机侧运维）。

  三条全局口径：
   * 改密成功后**后端撤销全部会话（含当前）**：前端登出并回登录页，
     抱着旧 cookie 继续跑只会在下一个请求 401；
   * Token 明文**仅签发响应里出现一次**：签发当时展示 + 复制 + 警示，
     关掉即无处可查（库里只有哈希）；
   * 写操作配 toast（成功 notice / 失败 error），读失败走 store.error
     错误条——与 S7.3–S7.7 各页同一套反馈范式。
-->
<template>
  <div class="st">
    <header class="st__head">
      <div>
        <h1 class="st__title">设置</h1>
        <p class="st__sub">账号安全、API Token、保护名单、TLS 指纹、整库备份与版本信息。改动保存后即时生效。</p>
      </div>
      <button type="button" class="btn" :disabled="st.loading || st.refreshing" @click="st.refresh()">刷新</button>
    </header>

    <p v-if="st.error" class="st__err">
      {{ st.error }}
      <button type="button" class="link" @click="st.load()">重试</button>
    </p>

    <!-- ① 账号安全：改密 -->
    <section class="card st__zone">
      <h2 class="st__zone-title">账号安全</h2>
      <p class="st__zone-desc">
        修改 <b>{{ auth.username || '当前账户' }}</b> 的登录密码。保存成功后全部会话（含当前）立即注销，需重新登录。
      </p>
      <form class="st__form" @submit.prevent="onChangePassword">
        <label class="field">
          <span class="field__label">当前密码</span>
          <input v-model="oldPw" class="field__input" type="password" autocomplete="current-password" required />
        </label>
        <label class="field">
          <span class="field__label">新密码</span>
          <input v-model="newPw" class="field__input" type="password" autocomplete="new-password" required />
          <span v-if="newPw && newPw.length < 8" class="field__hint field__hint--warn">新密码至少 8 位</span>
        </label>
        <label class="field">
          <span class="field__label">确认新密码</span>
          <input v-model="confirmPw" class="field__input" type="password" autocomplete="new-password" required />
          <span v-if="confirmPw && confirmPw !== newPw" class="field__hint field__hint--warn">两次输入不一致</span>
        </label>
        <div class="st__acts">
          <button type="submit" class="btn btn--primary" :disabled="!pwValid || changing">
            {{ changing ? '提交中…' : '修改密码' }}
          </button>
        </div>
      </form>
    </section>

    <!-- ② API Token 管理 -->
    <section class="card st__zone">
      <h2 class="st__zone-title">API Token 管理</h2>
      <p class="st__zone-desc">
        供 App / 脚本以 <code>Authorization: Bearer ddk_…</code> 调用。明文仅在签发响应里出现一次，请立即保存。
      </p>

      <!-- 签发表单 -->
      <form class="st__form st__tokform" @submit.prevent="onCreateToken">
        <label class="field">
          <span class="field__label">名称（唯一）</span>
          <input v-model="newTokName" class="field__input" type="text" autocomplete="off" placeholder="如 ios-shortcut" required />
        </label>
        <label class="field">
          <span class="field__label">有效期</span>
          <select v-model="newTokTtl" class="field__input">
            <option v-for="t in TOKEN_TTLS" :key="t" :value="t">{{ ttlLabel(t) }}</option>
          </select>
        </label>
        <fieldset class="st__scopes">
          <legend class="field__label">权限（scope，至少选一项）</legend>
          <label v-for="s in st.scopes" :key="s.scope" class="st__scope">
            <input v-model="newTokScopes" type="checkbox" :value="s.scope" />
            <span class="st__scope-name">{{ s.scope }}</span>
            <span class="st__scope-desc">{{ s.description }}</span>
          </label>
        </fieldset>
        <div class="st__acts">
          <button type="submit" class="btn btn--primary" :disabled="!tokValid || creating">
            {{ creating ? '签发中…' : '签发 Token' }}
          </button>
        </div>
      </form>

      <!-- 一次性明文：只在签发当时存在，关掉即无处可查 -->
      <div v-if="plaintext" class="st__plain">
        <div class="st__plain-head">
          <b>新 Token「{{ plaintext.name }}」的明文（仅此一次可见）</b>
          <button type="button" class="link" @click="plaintext = null">关闭</button>
        </div>
        <div class="st__plain-row">
          <code class="st__plain-text">{{ plaintext.text }}</code>
          <button type="button" class="btn" @click="copyText(plaintext.text, 'Token 明文')">复制</button>
        </div>
        <p class="st__plain-warn">请立即存入密码管理器。关闭本提示后无法再次查看；遗失只能撤销重签。</p>
      </div>

      <!-- 列表 -->
      <table v-if="st.tokens.length > 0" class="st__table">
        <thead>
          <tr>
            <th>名称</th>
            <th>权限</th>
            <th>创建时间</th>
            <th>最后使用</th>
            <th>过期</th>
            <th>状态</th>
            <th class="st__acts-col">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="t in st.tokens" :key="t.id">
            <td class="st__tokname">{{ t.name }}</td>
            <td class="st__tokscopes">{{ t.scopes.join('、') }}</td>
            <td class="st__when">{{ formatRFC3339(t.createdAt) }}</td>
            <td class="st__when">{{ t.lastUsedAt ? formatRFC3339(t.lastUsedAt) : '从未' }}</td>
            <td class="st__when">{{ t.expiresAt ? formatRFC3339(t.expiresAt) : '永不过期' }}</td>
            <td>
              <span class="st__badge" :class="t.active ? 'st__badge--ok' : 'st__badge--off'">{{
                t.active ? '可用' : t.revokedAt ? '已撤销' : '已过期'
              }}</span>
            </td>
            <td class="st__acts">
              <button v-if="t.active" type="button" class="link link--danger" @click="askRevoke(t)">撤销</button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-else class="st__empty">尚未签发任何 Token。</p>
    </section>

    <!-- ③ 保护名单 -->
    <section class="card st__zone">
      <h2 class="st__zone-title">保护名单</h2>
      <p class="st__zone-desc">
        正则、一行一条。命中的容器在 Hub 侧一律拒绝删除（403），请求不出门；保存即时生效，无需重启。
      </p>
      <p v-if="st.usingDefault" class="st__note">当前名单为空，实际使用默认名单 <code>^dockdeck-</code>（仅保护 DockDeck 自身）。</p>
      <textarea
        v-model="st.draftText"
        class="field__input st__list"
        rows="6"
        spellcheck="false"
        placeholder="^dockdeck-&#10;^watchtower$"
      ></textarea>
      <div class="st__acts">
        <button type="button" class="btn btn--primary" :disabled="!st.protectedDirty || saving" @click="onSaveProtected">
          {{ saving ? '保存中…' : '保存' }}
        </button>
        <button type="button" class="btn" :disabled="!st.protectedDirty || saving" @click="st.resetDraft()">恢复</button>
        <span v-if="st.protectedDirty" class="st__dirty">有未保存修改</span>
      </div>
    </section>

    <!-- ④ TLS 指纹 -->
    <section class="card st__zone">
      <h2 class="st__zone-title">TLS 指纹</h2>
      <template v-if="st.selfSigned">
        <p class="st__zone-desc">内置自签证书的 SHA-256 指纹。浏览器提示证书不受信任时，与页面地址栏显示的指纹逐字比对，一致再继续。</p>
        <div class="st__fp-row">
          <code class="st__fp">{{ st.fingerprint }}</code>
          <button type="button" class="btn" @click="copyText(st.fingerprint, 'TLS 指纹')">复制</button>
        </div>
      </template>
      <template v-else>
        <p class="st__zone-desc">当前未使用内置自签证书（TLS 已关闭，或由外部负载均衡终止）——没有指纹可供比对。</p>
      </template>
    </section>

    <!-- ⑤ 整库备份 -->
    <section class="card st__zone">
      <h2 class="st__zone-title">整库备份</h2>
      <p class="st__zone-desc">
        下载一份一致快照（SQLite VACUUM INTO 生成，中转文件用完即删）。快照含全部表：用户、会话、审计、受管服务器与各服务器加密令牌——
        <b>它等同于一份数据库副本，请按同等密级存放</b>。下载动作记入审计。
      </p>
      <div class="st__acts">
        <button type="button" class="btn btn--primary" :disabled="backingUp" @click="onBackup">
          {{ backingUp ? '生成中…' : '下载快照' }}
        </button>
      </div>
    </section>

    <!-- ⑥ 版本信息（2026-10-20 新增）：Hub 构建元数据，GET /system/version。
         拉不到只灰显本区（旧版 Hub 无此端点会 404），不影响其他分区。 -->
    <section class="card st__zone">
      <h2 class="st__zone-title">版本信息</h2>
      <p class="st__zone-desc">当前 Hub 的构建版本（构建时注入）。Agent 版本请在目标机执行 <code>docker inspect</code> 查看镜像标签。</p>
      <dl class="st__ver">
        <div class="st__ver-row">
          <dt>版本</dt>
          <dd>{{ build?.version || '—' }}</dd>
        </div>
        <div class="st__ver-row">
          <dt>Commit</dt>
          <dd><code>{{ build?.commit || '—' }}</code></dd>
        </div>
        <div class="st__ver-row">
          <dt>构建时间</dt>
          <dd>{{ build?.date || '—' }}</dd>
        </div>
      </dl>
      <p v-if="verError" class="st__ver-err">版本信息获取失败：{{ verError }}</p>
    </section>

    <!-- 撤销 Token：简单确认（破坏性——撤后该凭据即刻失效） -->
    <AppConfirm
      :open="revokeTarget !== null"
      title="撤销 Token"
      :message="`撤销后 ${revokeTarget ? revokeTarget.name : ''} 立即失效，使用它的 App / 脚本将收 401。已产生的审计记录保留。`"
      danger
      confirm-label="撤销"
      @confirm="onConfirmRevoke"
      @cancel="revokeTarget = null"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import AppConfirm from '@/components/AppConfirm.vue'
import { ApiError } from '@/api/errors'
import { changePassword } from '@/api/auth'
import { downloadBackup, putProtectedContainers } from '@/api/settings'
import { fetchBuildInfo, type BuildInfo } from '@/api/system'
import { TOKEN_TTLS, createToken, revokeToken, type ApiToken, type TokenTtl } from '@/api/tokens'
import { useSettingsStore } from '@/stores/settings'
import { useAuthStore } from '@/stores/auth'
import { useUiStore } from '@/stores/ui'
import { formatRFC3339 } from '@/utils/format'

const st = useSettingsStore()
const auth = useAuthStore()
const ui = useUiStore()
const router = useRouter()

onMounted(() => void st.load())

/* ---------- ⑥ 版本信息：与 st.load() 并行，互不阻塞 ---------- */

const build = ref<BuildInfo | null>(null)
const verError = ref('')

void fetchBuildInfo()
  .then((b) => {
    build.value = b
  })
  .catch((e: unknown) => {
    verError.value = e instanceof Error ? e.message : String(e)
  })

/* ---------- ① 改密 ---------- */

const oldPw = ref('')
const newPw = ref('')
const confirmPw = ref('')
const changing = ref(false)

/** 与后端弱密码判定同口径（至少 8 位）+ 两次一致 + 旧密码非空。 */
const pwValid = computed(
  () => oldPw.value.length > 0 && newPw.value.length >= 8 && confirmPw.value === newPw.value,
)

async function onChangePassword(): Promise<void> {
  if (!pwValid.value || changing.value) {
    return
  }
  changing.value = true
  try {
    await changePassword(oldPw.value, newPw.value)
    ui.notifyNotice('密码已修改，全部会话已注销——请用新密码重新登录')
    // 后端已撤销全部会话（含当前）：先清前端态再回登录页。
    // apiLogout 此时多半 401，auth store 的 logout 失败也重置本地态。
    await auth.logout()
    await router.push({ name: 'login' })
  } catch (e) {
    // invalid_credentials（旧密码错）/ weak_password 的 message 直接透出。
    ui.notifyError(e)
  } finally {
    changing.value = false
  }
}

/* ---------- ② API Token ---------- */

const newTokName = ref('')
const newTokTtl = ref<TokenTtl>('90d')
const newTokScopes = ref<string[]>([])
const creating = ref(false)
/** 签发响应里的一次性明文；null = 未展示/已关闭。 */
const plaintext = ref<{ name: string; text: string } | null>(null)

const tokValid = computed(() => newTokName.value.trim().length > 0 && newTokScopes.value.length > 0)

function ttlLabel(t: TokenTtl): string {
  switch (t) {
    case '7d':
      return '7 天'
    case '30d':
      return '30 天'
    case '90d':
      return '90 天'
    case '1y':
      return '1 年'
    case 'never':
      return '永不过期'
  }
}

async function onCreateToken(): Promise<void> {
  if (!tokValid.value || creating.value) {
    return
  }
  creating.value = true
  try {
    const r = await createToken({
      name: newTokName.value.trim(),
      scopes: [...newTokScopes.value],
      ttl: newTokTtl.value,
    })
    plaintext.value = { name: r.token.name, text: r.plaintext }
    ui.notifyNotice(`已签发 ${r.token.name}；明文仅此一次可见，请立即保存`)
    newTokName.value = ''
    newTokScopes.value = []
    await st.refreshTokens()
  } catch (e) {
    // name_taken（409）/ bad_request / bad_ttl 的 message 直接透出。
    ui.notifyError(e)
  } finally {
    creating.value = false
  }
}

const revokeTarget = ref<ApiToken | null>(null)

function askRevoke(t: ApiToken): void {
  revokeTarget.value = t
}

async function onConfirmRevoke(): Promise<void> {
  const t = revokeTarget.value
  revokeTarget.value = null
  if (!t) {
    return
  }
  try {
    await revokeToken(t.id)
    ui.notifyNotice(`已撤销 ${t.name}`)
    await st.refreshTokens()
  } catch (e) {
    ui.notifyError(e)
  }
}

/* ---------- ③ 保护名单 ---------- */

const saving = ref(false)

async function onSaveProtected(): Promise<void> {
  // 注意：store 实例上的 ref 已解包——st.protectedDirty 直接是 boolean，
  // 没有 .value（模板与脚本同口径，只有 store 内部才用 .value）。
  if (!st.protectedDirty || saving.value) {
    return
  }
  saving.value = true
  try {
    // 发的是归一后的 draft（trim/去空行）；坏正则 400 的「第 N 条」文案透出。
    const v = await putProtectedContainers([...st.draftPatterns])
    st.applyProtectedView(v)
    ui.notifyNotice(`保护名单已保存（${v.patterns.length} 条），即时生效`)
  } catch (e) {
    ui.notifyError(e)
  } finally {
    saving.value = false
  }
}

/* ---------- ⑤ 备份 ---------- */

const backingUp = ref(false)

async function onBackup(): Promise<void> {
  if (backingUp.value) {
    return
  }
  backingUp.value = true
  try {
    const name = await downloadBackup()
    ui.notifyNotice(`快照已下载：${name}（请按数据库副本同等保管）`)
  } catch (e) {
    ui.notifyError(e)
  } finally {
    backingUp.value = false
  }
}

/* ---------- 剪贴板 ---------- */

/** TLS 指纹与 Token 明文共用。失败不静默——提示用户手动选择复制。 */
async function copyText(text: string, label: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text)
    ui.notifyNotice(`已复制${label}`)
  } catch {
    ui.notifyError(new ApiError(0, 'clipboard_error', '浏览器拒绝了剪贴板访问，请手动选择文本复制'))
  }
}
</script>

<style scoped>
.st {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.st__head {
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
}

.st__title {
  font-size: var(--text-xl);
}

.st__sub {
  margin-top: var(--space-1);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.st__head .btn {
  margin-left: auto;
  flex: none;
}

.st__err {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-3);
  background: var(--danger-subtle);
  border: 1px solid var(--danger-border);
  border-radius: var(--radius-md);
  color: var(--danger);
  font-size: var(--text-sm);
}

/* ---------- 分区卡片 ---------- */

.st__zone {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.st__zone-title {
  font-size: var(--text-base);
  font-weight: var(--weight-semibold);
}

.st__zone-desc {
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.st__zone-desc code {
  font-family: var(--font-mono);
  font-size: var(--text-xs);
}

.st__note {
  padding: var(--space-2) var(--space-3);
  background: var(--bg-sunken);
  border-radius: var(--radius-sm);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.st__note code {
  font-family: var(--font-mono);
}

.st__empty {
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.st__acts {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

.st__dirty {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

.st__acts-col {
  width: 72px;
}

/* ---------- 表单 ---------- */

.st__form {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  max-width: 420px;
}

.st__tokform {
  max-width: none;
}

.st__scopes {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
  gap: var(--space-2);
  margin: 0;
  padding: var(--space-3);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-md);
}

.st__scope {
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
  font-size: var(--text-sm);
}

.st__scope-name {
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  white-space: nowrap;
}

.st__scope-desc {
  color: var(--fg-subtle);
  font-size: var(--text-xs);
}

/* ---------- Token 一次性明文 ---------- */

.st__plain {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding: var(--space-3);
  background: var(--bg-sunken);
  border: 1px solid var(--success-border);
  border-radius: var(--radius-md);
}

.st__plain-head {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  font-size: var(--text-sm);
}

.st__plain-head .link {
  margin-left: auto;
}

.st__plain-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

.st__plain-text {
  flex: 1;
  min-width: 0;
  padding: var(--space-2);
  background: var(--bg-elev-1);
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  word-break: break-all;
}

.st__plain-warn {
  color: var(--fg-muted);
  font-size: var(--text-xs);
}

/* ---------- 表格 ---------- */

.st__table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--text-sm);
}

.st__table th {
  padding: var(--space-2);
  border-bottom: 1px solid var(--border-default);
  color: var(--fg-subtle);
  font-weight: var(--weight-normal);
  text-align: left;
}

.st__table td {
  padding: var(--space-2);
  border-bottom: 1px solid var(--border-subtle);
  vertical-align: middle;
}

.st__tokname {
  font-weight: var(--weight-medium);
}

.st__tokscopes {
  color: var(--fg-muted);
  font-size: var(--text-xs);
}

.st__when {
  color: var(--fg-muted);
  white-space: nowrap;
}

.st__acts {
  white-space: nowrap;
}

.st__badge {
  display: inline-block;
  padding: 0 var(--space-2);
  border-radius: var(--radius-sm);
  font-size: var(--text-xs);
  line-height: 20px;
}

.st__badge--ok {
  background: var(--success-subtle);
  border: 1px solid var(--success-border);
  color: var(--success);
}

.st__badge--off {
  background: var(--bg-sunken);
  border: 1px solid var(--border-default);
  color: var(--fg-subtle);
}

/* ---------- 保护名单 textarea / TLS 指纹 ---------- */

.st__list {
  font-family: var(--font-mono);
  font-size: var(--text-sm);
  resize: vertical;
}

.st__fp-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

.st__fp {
  flex: 1;
  min-width: 0;
  padding: var(--space-2);
  background: var(--bg-sunken);
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: var(--text-sm);
  word-break: break-all;
}

/* ---------- ⑥ 版本信息 ---------- */

.st__ver {
  display: grid;
  gap: var(--space-2);
  margin: 0;
}

.st__ver-row {
  display: flex;
  gap: var(--space-3);
  align-items: baseline;
  font-size: var(--text-sm);
}

.st__ver-row dt {
  flex: 0 0 5.5rem;
  color: var(--fg-subtle);
}

.st__ver-row dd {
  margin: 0;
  font-family: var(--font-mono);
  word-break: break-all;
}

.st__ver-err {
  margin: var(--space-3) 0 0;
  color: var(--danger);
  font-size: var(--text-sm);
}
</style>
