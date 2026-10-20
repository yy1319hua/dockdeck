<!--
  登录页。错误走全局 AppToast（S6.2 建立的统一错误通道），本页不再另做一套错误区，
  免得同一条错误出现两处。

  刻意不做的几件事：
    * 不校验用户名/密码格式——后端对「用户不存在」与「密码错」统一回 invalid_credentials
      以免泄漏用户名是否存在，前端若再提示「用户名不存在」等于把后端的设计抹掉。
    * 不在本页显示 AccountLocked / AccountDisabled 的差异化文案——这些语义由后端 message 承载。
    * 不无条件显示「创建首个账户」——仅 GET /auth/me 回报 setup_required=true（系统无账户）时
      才显示；已建号的系统再挂这个入口，等于告诉访客「这系统还没设防」。
-->
<template>
  <div class="login">
    <form class="card login__card" @submit.prevent="onSubmit">
      <header class="login__head">
        <h1 class="login__title">登录 DockDeck</h1>
        <p class="login__lead">会话仅限浏览器使用；API Token 不能调用认证端点。</p>
      </header>

      <label class="field">
        <span class="field__label">用户名</span>
        <input
          v-model.trim="username"
          class="field__input"
          type="text"
          name="username"
          autocomplete="username"
          placeholder="请输入用户名"
          :disabled="auth.pending"
          required
        />
      </label>

      <label class="field">
        <span class="field__label">密码</span>
        <input
          v-model="password"
          class="field__input"
          type="password"
          name="password"
          autocomplete="current-password"
          placeholder="请输入密码"
          :disabled="auth.pending"
          required
        />
      </label>

      <button
        type="submit"
        class="btn btn--primary btn--block login__submit"
        :disabled="auth.pending || !canSubmit"
      >
        {{ auth.pending ? '登录中…' : '登录' }}
      </button>

      <footer v-if="setupRequired" class="login__foot">
        <span>系统还没有账户？</span>
        <RouterLink to="/setup">创建首个账户</RouterLink>
      </footer>
    </form>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { useUiStore } from '@/stores/ui'
import { fetchMe } from '@/api/auth'

const auth = useAuthStore()
const ui = useUiStore()
const route = useRoute()
const router = useRouter()

const username = ref('')
const password = ref('')

// 仅当系统尚无任何账户时才显示「创建首个账户」入口：GET /auth/me 未登录也返 200，
// setup_required=true 即「该建号」。探测失败一律按 false 处理（有账户也不该看见入口；
// 真没有账户时 /setup 页自身仍可建号，这里不挡路）。
const setupRequired = ref(false)
onMounted(() => {
  fetchMe()
    .then((me) => {
      setupRequired.value = me.setup_required
    })
    .catch(() => {
      setupRequired.value = false
    })
})

const canSubmit = computed(() => username.value !== '' && password.value !== '')

/** 只接受站内路径：单斜杠开头，且排除 //host 这种协议相对地址（开放重定向）。 */
function safeRedirect(): string {
  const raw = route.query.redirect
  if (typeof raw !== 'string') {
    return '/'
  }
  return raw.startsWith('/') && !raw.startsWith('//') ? raw : '/'
}

async function onSubmit() {
  ui.clearError()
  try {
    await auth.login({ username: username.value, password: password.value })
    await router.replace(safeRedirect())
  } catch (e) {
    // 交给统一通道呈现（含 invalid_credentials / account_locked / account_disabled）。
    ui.notifyError(e)
  } finally {
    password.value = ''
  }
}
</script>

<style scoped>
.login {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 100vh;
  padding: var(--space-5);
}

.login__card {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  width: 100%;
  max-width: 380px;
}

.login__title {
  font-size: var(--text-xl);
}

.login__lead {
  margin-top: var(--space-2);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.login__submit {
  margin-top: var(--space-2);
}

.login__foot {
  display: flex;
  justify-content: center;
  gap: var(--space-2);
  color: var(--fg-subtle);
  font-size: var(--text-sm);
}
</style>
