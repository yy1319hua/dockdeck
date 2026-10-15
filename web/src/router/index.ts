import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import HomePlaceholder from '@/pages/HomePlaceholder.vue'

// 路由表。S6.2 起随功能页扩展；页面与 API 层的鉴权守卫在 S6.2 补。
const routes: RouteRecordRaw[] = [
  {
    path: '/',
    name: 'home',
    component: HomePlaceholder,
    meta: { title: 'DockDeck' },
  },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})

export default router
