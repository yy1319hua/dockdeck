import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import HomePlaceholder from '@/pages/HomePlaceholder.vue'
import DesignTokens from '@/pages/DesignTokens.vue'

// 路由表。S6.2 起随功能页扩展；页面级的鉴权守卫在 S6.2 补。
const routes: RouteRecordRaw[] = [
  {
    path: '/',
    name: 'home',
    component: HomePlaceholder,
    meta: { title: 'DockDeck' },
  },
  {
    // 设计令牌色卡页：开发期自检用，S6.3 之后可从生产路由表摘除。
    path: '/dev/tokens',
    name: 'design-tokens',
    component: DesignTokens,
    meta: { title: 'Design Tokens · DockDeck' },
  },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})

export default router
