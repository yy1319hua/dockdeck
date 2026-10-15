import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import HomePlaceholder from '@/pages/HomePlaceholder.vue'
import DesignTokens from '@/pages/DesignTokens.vue'
import LoginPlaceholder from '@/pages/LoginPlaceholder.vue'
import SetupPlaceholder from '@/pages/SetupPlaceholder.vue'

declare module 'vue-router' {
  interface RouteMeta {
    title?: string
    /**
     * true = 需登录才能访问。
     * /login 与 /setup 必须留空（公开），否则守卫会把未登录用户反复重定向到自身而成环。
     */
    requiresAuth?: boolean
  }
}

const routes: RouteRecordRaw[] = [
  {
    path: '/',
    name: 'home',
    component: HomePlaceholder,
    // S6.4 已定：状态与数据须登录才显示。本轮先由守卫把未登录挡到 /login。
    meta: { title: 'DockDeck', requiresAuth: true },
  },
  {
    path: '/login',
    name: 'login',
    component: LoginPlaceholder,
    meta: { title: '登录 · DockDeck' },
  },
  {
    path: '/setup',
    name: 'setup',
    component: SetupPlaceholder,
    meta: { title: '初始化 · DockDeck' },
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
