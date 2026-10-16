/*
 * 侧栏导航的单一数据源：任何地方要改菜单项都只动这里。
 *
 * to 为空 = 功能页尚未接入（属 S7），渲染为置灰不可点并带「待实现」标记；
 * 这样布局在 S6 阶段就能定型，不必等 S7 逐个页面做完再回头改布局文件。
 */
import type { IconName } from './icons'

export interface NavItem {
  label: string
  icon: IconName
  /** 有路由名即可跳转；为空表示该页未接入。 */
  to?: string
}

export const navItems: NavItem[] = [
  { label: '总览', icon: 'overview', to: 'home' },
  // 主机详情是按机维度页（/servers/:id，要 id），侧栏无法静态链接——
  // 从总览卡片进入；此项保持置灰，不做假链接。
  { label: '主机详情', icon: 'host' },
  { label: '容器', icon: 'container', to: 'containers' },
  { label: '镜像', icon: 'image', to: 'images' },
  { label: 'Compose', icon: 'compose', to: 'compose' },
  // S7.9：卷与网络是一页两分区（Q1-A），故只占一个侧栏项。
  { label: '卷与网络', icon: 'volnet', to: 'volumes-networks' },
  { label: '文件', icon: 'files', to: 'files' },
  { label: '审计', icon: 'audit', to: 'audit' },
  { label: '设置', icon: 'settings', to: 'settings' },
]
