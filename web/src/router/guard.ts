import type { Router } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { useUiStore } from '@/stores/ui'

/*
 * 路由守卫：进任何页面前先确认「我是谁」。
 *
 * 三条规则，顺序不能换：
 *   1. 状态未知（尚未探测）→ 先探测；失败只报错、不重定向（网络抖一下就踢到登录页太粗暴）。
 *   2. 系统尚无账户（setup_required）→ 去建号页。
 *   3. 已证实未登录且目标是受保护页 → 去登录页并带上 redirect。
 *
 * 公开路由（/login、/setup、/dev/tokens）在第 2、3 步之前就放行，故不会自重定向成环。
 */
export function installGuard(router: Router): void {
  router.beforeEach(async (to) => {
    const auth = useAuthStore()
    const ui = useUiStore()

    if (!auth.loaded) {
      ui.beginLoading()
      try {
        await auth.probe()
      } catch (e) {
        ui.notifyError(e)
      } finally {
        ui.endLoading()
      }
    }

    if (to.meta.requiresAuth !== true) {
      return true
    }

    if (auth.setupRequired) {
      return { name: 'setup' }
    }

    if (!auth.signedIn) {
      return { name: 'login', query: { redirect: to.fullPath } }
    }

    return true
  })
}
