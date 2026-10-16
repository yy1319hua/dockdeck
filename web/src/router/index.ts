import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import AppShell from '@/components/layout/AppShell.vue'
import Overview from '@/pages/Overview.vue'
import HostDetail from '@/pages/HostDetail.vue'
import Containers from '@/pages/Containers.vue'
import Images from '@/pages/Images.vue'
import Compose from '@/pages/Compose.vue'
import Files from '@/pages/Files.vue'
import DesignTokens from '@/pages/DesignTokens.vue'
import Login from '@/pages/Login.vue'
import Setup from '@/pages/Setup.vue'

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

/*
 * 谁套外壳由路由层级决定，不在 App.vue 里做条件判断：
 *   / 及其子路由  → 父组件是 AppShell（侧栏 + 顶栏 + 内容）
 *   /login /setup → 顶层路由，天然不套壳，保持居中单卡
 * children 的 path 一律不带前导斜杠，否则会脱离父级、退回绝对路径。
 */
const routes: RouteRecordRaw[] = [
  {
    path: '/',
    component: AppShell,
    children: [
      {
        path: '',
        name: 'home',
        component: Overview,
        // S6.4 已定：状态与数据须登录才显示。未登录由守卫挡到 /login。
        meta: { title: '总览 · DockDeck', requiresAuth: true },
      },
      {
        path: 'servers/:id',
        name: 'host-detail',
        component: HostDetail,
        meta: { title: '主机详情 · DockDeck', requiresAuth: true },
      },
      {
        // S7.3 容器页：全局页 + 顶部主机切换器（清单按机维度，切换在页内做）。
        path: 'containers',
        name: 'containers',
        component: Containers,
        meta: { title: '容器 · DockDeck', requiresAuth: true },
      },
      {
        // S7.4 镜像页：全局页 + 顶部主机切换器（与容器页同构，用户拍板）。
        path: 'images',
        name: 'images',
        component: Images,
        meta: { title: '镜像 · DockDeck', requiresAuth: true },
      },
      {
        // S7.5 Compose 页：全局页 + 顶部主机切换器（同构）；编辑面板与两段式写入在页内。
        path: 'compose',
        name: 'compose',
        component: Compose,
        meta: { title: 'Compose · DockDeck', requiresAuth: true },
      },
      {
        // S7.6 文件页：全局页 + 顶部主机切换器（同构）；目录位置进 store（切走回来停原目录），
        // 编辑态是页面本地 ref。无轮询——文件非实时数据，只留手动刷新。
        path: 'files',
        name: 'files',
        component: Files,
        meta: { title: '文件 · DockDeck', requiresAuth: true },
      },
      {
        // 设计令牌色卡页：开发期自检用。
        path: 'dev/tokens',
        name: 'design-tokens',
        component: DesignTokens,
        meta: { title: 'Design Tokens · DockDeck' },
      },
    ],
  },
  {
    path: '/login',
    name: 'login',
    component: Login,
    meta: { title: '登录 · DockDeck' },
  },
  {
    path: '/setup',
    name: 'setup',
    component: Setup,
    meta: { title: '创建首个账户 · DockDeck' },
  },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})

export default router
