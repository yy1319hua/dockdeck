<!--
  S6.2 占位建号页：系统尚无任何账户时（GET /auth/me 返回 setup_required=true），
  守卫会把用户送到这里。真实表单属 S6.4。
-->
<template>
  <div class="auth">
    <div class="auth__card">
      <h1 class="auth__title">创建首个账户</h1>
      <p class="auth__hint">
        当前系统还没有任何账户，需要先完成初始化。表单在 S6.4 接入（接口已就绪：<code>POST /api/v1/auth/setup</code>）。
      </p>
      <RouterLink class="auth__link" to="/login">前往登录 →</RouterLink>
    </div>
  </div>
</template>

<script setup lang="ts">
// 占位页暂无逻辑。
</script>

<style scoped>
.auth {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 100vh;
  padding: var(--space-5);
}

.auth__card {
  width: 100%;
  max-width: 380px;
  padding: var(--space-6);
  background: var(--bg-elev-1);
  border: 1px solid var(--border-default);
  border-radius: var(--radius-lg);
  box-shadow: var(--shadow-md);
  text-align: center;
}

.auth__title {
  font-size: var(--text-xl);
}

.auth__hint {
  margin-top: var(--space-3);
  color: var(--fg-muted);
  font-size: var(--text-sm);
}

.auth__hint code {
  color: var(--fg-default);
}

.auth__link {
  display: inline-block;
  margin-top: var(--space-5);
  font-size: var(--text-sm);
}
</style>
