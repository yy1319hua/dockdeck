import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { toErrorInfo } from '@/api/errors'

/*
 * 全局 UI 态：请求计数式加载态 + 最近一次错误。
 *
 * 加载态用「计数」而不是布尔：并发请求时任一未结束都应显示加载，
 * 若用布尔会被先返回的那个请求提前关掉。
 */
export const useUiStore = defineStore('ui', () => {
  const ready = ref(false)
  const loading = ref(0)
  const error = ref<{ code: string; message: string } | null>(null)

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
  }
})
