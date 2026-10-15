import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { fetchMe, type Credentials, login as apiLogin, logout as apiLogout, setup as apiSetup } from '@/api/auth'

/*
 * 鉴权状态。唯一事实来源是 GET /auth/me —— 它未登录也返回 200，
 * 用 setup_required 区分「系统还没账户，该建号」与「已有账户，该登录」。
 * 会话本身由 Hub 的 cookie 持有，前端不落 localStorage（避免多一份易失副本）。
 */
export const useAuthStore = defineStore('auth', () => {
  const username = ref('')
  const setupRequired = ref(false)
  /** 是否已成功探测过一次；false 表示状态未知，守卫会先探测。 */
  const loaded = ref(false)
  const pending = ref(false)

  const signedIn = computed(() => username.value !== '')

  /** 探测当前身份。force=true 用于登录后立刻刷新。失败时不吞错误，交给守卫展示。 */
  async function probe(force = false): Promise<void> {
    if (loaded.value && !force) {
      return
    }
    pending.value = true
    try {
      const me = await fetchMe()
      username.value = me.username
      setupRequired.value = me.setup_required
      loaded.value = true
    } finally {
      pending.value = false
    }
  }

  async function login(cred: Credentials): Promise<void> {
    pending.value = true
    try {
      await apiLogin(cred)
      await probe(true)
    } finally {
      pending.value = false
    }
  }

  async function setup(cred: Credentials): Promise<void> {
    pending.value = true
    try {
      await apiSetup(cred)
      await probe(true)
    } finally {
      pending.value = false
    }
  }

  async function logout(): Promise<void> {
    try {
      await apiLogout()
    } finally {
      // 登出无论成功与否都按未登录重置：cookie 已失效或无意义。
      username.value = ''
      setupRequired.value = false
      loaded.value = true
    }
  }

  return {
    username,
    setupRequired,
    loaded,
    pending,
    signedIn,
    probe,
    login,
    setup,
    logout,
  }
})
