/*
 * 图标：自制 24×24 描边路径，零依赖、不引图标库（项目不引重型框架）。
 * 只用几何笔划，不塞商业图标库的造型，避免许可与体积负担。
 */
export type IconName =
  | 'overview'
  | 'host'
  | 'container'
  | 'image'
  | 'compose'
  | 'volnet'
  | 'files'
  | 'audit'
  | 'settings'
  | 'menu'

export const icons: Record<IconName, string[]> = {
  overview: ['M3 3h7v7H3zM14 3h7v7h-7zM3 14h7v7H3zM14 14h7v7h-7z'],
  host: ['M3 4h18v6H3zM3 14h18v6H3z', 'M7 7h.01M7 17h.01'],
  container: ['M21 8v8a2 2 0 0 1-1 1.73l-7 4a2 2 0 0 1-2 0l-7-4A2 2 0 0 1 3 16V8a2 2 0 0 1 1-1.73l7-4a2 2 0 0 1 2 0l7 4A2 2 0 0 1 21 8z'],
  image: ['M12 3a9 9 0 1 0 9 9', 'M12 7a5 5 0 1 1-5 5', 'M12 12h.01'],
  compose: ['M8 6h13M8 12h13M8 18h13', 'M3 6h.01M3 12h.01M3 18h.01'],
  // 卷与网络（S7.9）：左半是卷（圆柱，两道椭圆线），右半是网络
  // （三点加连线）——一图标兼表两分区，与侧栏「卷与网络」同义。
  volnet: [
    'M3 7c0-1.66 2.24-3 5-3s5 1.34 5 3-2.24 3-5 3-5-1.34-5-3z',
    'M3 7v10c0 1.66 2.24 3 5 3s5-1.34 5-3V7',
    'M15 8h.01M15 16h.01M21 12h.01',
    'M15 8l6 4-6 4',
  ],
  files: ['M4 4h6l2 2h8v12H4z', 'M14 6V4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8'],
  audit: ['M14 3H6a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z', 'M14 3v6h6', 'M8 13h6M8 17h4'],
  settings: ['M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3', 'M1 14h6M9 8h6M17 16h6'],
  menu: ['M3 6h18M3 12h18M3 18h18'],
}
