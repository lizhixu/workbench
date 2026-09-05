import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// https://vite.dev/config/
export default defineConfig({
  plugins: [vue()],
  build: {
    rolldownOptions: {
      output: {
        // 三个重型库单独成包，避免它们被并进某个路由 chunk 后，
        // 任何引用该路由的页面都要整包下载。
        // 其余依赖沿用默认切分：naive-ui 若被合成单包，登录页也要背上
        // 整个组件库（约 256KB gzip），首屏反而更慢。
        advancedChunks: {
          groups: [
            { name: 'vendor-xterm', test: /node_modules[\\/]@xterm[\\/]/, priority: 30 },
            { name: 'vendor-asciinema', test: /node_modules[\\/]asciinema-player[\\/]/, priority: 30 },
            { name: 'vendor-vue', test: /node_modules[\\/](vue|@vue|vue-router|pinia)[\\/]/, priority: 10 },
          ],
        },
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      // Forward REST API and WebSocket to the control server during dev.
      '/api': {
        target: 'http://localhost:18080',
        changeOrigin: true,
        ws: true,
      },
    },
  },
})
