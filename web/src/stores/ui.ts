import { defineStore } from 'pinia'
import { ref } from 'vue'

// 全局 UI 态的最小样例：证明 pinia 接线可用，S6.3 布局用它管侧栏折叠等。
export const useUiStore = defineStore('ui', () => {
  const ready = ref(false)

  function markReady() {
    ready.value = true
  }

  return { ready, markReady }
})
