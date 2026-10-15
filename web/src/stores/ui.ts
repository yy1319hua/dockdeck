import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { toErrorInfo } from '@/api/errors'

interface ErrorInfo {
  code: string
  message: string
}

/*
 * 全局 UI 态：请求计数式加载态 + 最近一次错误 + 侧栏两态。
 *
 * 加载态用「计数」而不是布尔：并发请求时任一未结束都应显示加载，
 * 若用布尔会被先返回的那个请求提前关掉。
 *
 * 侧栏分两个独立状态，不要合并：
 *   sidebarOpen     —— 移动端的抽屉（覆盖层），路由切换后自动关。
 *   sidebarCollapsed—— 桌面端的折叠（图标条），是用户长期偏好，不该被路由切换重置。
 */
export const useUiStore = defineStore('ui', () => {
  const ready = ref(false)
  const loading = ref(0)
  const error = ref<ErrorInfo | null>(null)
  /** 操作成功的轻提示（S7.3 起：容器操作的结果反馈）。与 error 分开——失败是 alert，成功是 status。 */
  const notice = ref<string | null>(null)
  let noticeTimer: ReturnType<typeof setTimeout> | undefined
  const sidebarOpen = ref(false)
  const sidebarCollapsed = ref(false)

  const isLoading = computed(() => loading.value > 0)

  function markReady() {
    ready.value = true
  }

  function beginLoading() {
    loading.value += 1
  }

  function endLoading() {
    loading.value = loading.value > 0 ? loading.value - 1 : 0
  }

  function notifyError(e: unknown) {
    error.value = toErrorInfo(e)
  }

  function clearError() {
    error.value = null
  }

  /** 成功提示数秒后自熄；连续成功时重置计时，不堆积。 */
  function notifyNotice(message: string) {
    notice.value = message
    if (noticeTimer !== undefined) {
      clearTimeout(noticeTimer)
    }
    noticeTimer = setTimeout(() => {
      notice.value = null
      noticeTimer = undefined
    }, 4000)
  }

  function clearNotice() {
    notice.value = null
    if (noticeTimer !== undefined) {
      clearTimeout(noticeTimer)
      noticeTimer = undefined
    }
  }

  function openSidebar() {
    sidebarOpen.value = true
  }

  function closeSidebar() {
    sidebarOpen.value = false
  }

  function toggleSidebar() {
    sidebarOpen.value = !sidebarOpen.value
  }

  function toggleCollapsed() {
    sidebarCollapsed.value = !sidebarCollapsed.value
  }

  return {
    ready,
    markReady,
    loading,
    isLoading,
    beginLoading,
    endLoading,
    error,
    notifyError,
    clearError,
    notice,
    notifyNotice,
    clearNotice,
    sidebarOpen,
    sidebarCollapsed,
    openSidebar,
    closeSidebar,
    toggleSidebar,
    toggleCollapsed,
  }
})
