<!--
  首启建号页：系统尚无任何账户时（GET /auth/me 返回 setup_required=true），
  守卫会把用户送到这里；也可手工访问。

  前端唯一复用的硬规则是「口令至少 8 位」——这是后端 Setup 里唯一的长度约束
  （< 8 位报 weak_password）。本地先拦一次，省掉一次往返；后端仍会复查。
-->
<template>
  <div class="setup">
    <form class="card setup__card" @submit.prevent="onSubmit">
      <header class="setup__head">
        <h1 class="setup__title">创建首个账户</h1>
        <p class="setup__lead">这是系统的第一个账户，创建后即登录。账户建成后建号通道自动关闭。</p>
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
        <span class="field__label">口令</span>
        <input
          v-model="password"
          class="field__input"
          type="password"
          name="password"
          autocomplete="new-password"
          placeholder="至少 8 位"
          :disabled="auth.pending"
          required
        />
        <span v-if="tooShort" class="field__hint field__hint--warn">口令至少 8 位</span>
      </label>

      <label class="field">
        <span class="field__label">确认口令</span>
        <input
          v-model="confirm"
          class="field__input"
          type="password"
          name="confirm"
          autocomplete="new-password"
          placeholder="再输入一次"
          :disabled="auth.pending"
          required
        />
        <span v-if="mismatch" class="field__hint field__hint--warn">两次输入的口令不一致</span>
      </label>

      <button
        type="submit"
        class="btn btn--primary btn--block setup__submit"
        :disabled="auth.pending || !canSubmit"
      >
        {{ auth.pending ? '创建中…' : '创建并登录' }}
      </button>

      <footer class="setup__foot">
        <span>已经有账户了？</span>
        <RouterLink to="/login">去登录</RouterLink>
      </footer>
    </form>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { useUiStore } from '@/stores/ui'

const auth = useAuthStore()
const ui = useUiStore()
const router = useRouter()

const username = ref('')
const password = ref('')
const confirm = ref('')

const tooShort = computed(() => password.value !== '' && password.value.length < 8)
const mismatch = computed(() => confirm.value !== '' && confirm.value !== password.value)

const canSubmit = computed(
  () => username.value !== '' && password.value.length >= 8 && confirm.value === password.value,
)

async function onSubmit() {
  ui.clearError()
  try {
    await auth.setup({ username: username.value, password: password.value })
    await router.replace('/')
  } catch (e) {
    // 409 already_initialized 之类由后端 message 承载，统一通道呈现。
    ui.notifyError(e)
  } finally {
    password.value = ''
    confirm.value = ''
  }
}
</script>

<style scoped>
.setup {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 100vh;
  padding: var(--space-5);
}

.setup__card {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  width: 100%;
  max-width: 380px;
}

.setup__title {
  font-size: var(--text-xl);
}

.setup__lead {
  margin-top: var(--space-2);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.setup__submit {
  margin-top: var(--space-2);
}

.setup__foot {
  display: flex;
  justify-content: center;
  gap: var(--space-2);
  color: var(--fg-subtle);
  font-size: var(--text-sm);
}
</style>
