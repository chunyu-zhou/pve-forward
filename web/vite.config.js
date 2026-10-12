import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发时把 /api 与 /agent 代理到本地控制端（pve-forward -listen :5080 且 TLS=proxy）
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5174,
    proxy: {
      '/api': { target: process.env.PF_BACKEND || 'http://127.0.0.1:5080', changeOrigin: true },
      '/agent': { target: process.env.PF_BACKEND || 'http://127.0.0.1:5080', changeOrigin: true, ws: true },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 2000,
  },
})
