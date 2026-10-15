import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// S6 前端骨架。构建产物落在 web/dist，由 Go 侧 go:embed 接进 Hub 二进制（见 internal/assets）。
// 开发期跑 vite dev server（GAP-5 裁定），/api 反代到本机 Hub；
// 生产不走本代理——Hub 同源提供 SPA 与 /api。
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    port: 5173,
    strictPort: false,
    proxy: {
      '/api': {
        target: 'https://localhost:8443',
        // Hub 开发期用自签 TLS；此开关仅影响本地代理，不作用于生产构建。
        secure: false,
        changeOrigin: true,
      },
    },
  },
})
