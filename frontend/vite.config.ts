import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// During `npm run dev` the SPA talks to /api. Each service is proxied
// separately so the four Go services can run on distinct local ports.
const proxyTargets = {
  '/api/auth': process.env.CHIRP_AUTH_URL ?? 'http://localhost:8081',
  '/api/posts': process.env.CHIRP_POSTS_URL ?? 'http://localhost:8082',
  '/api/uploads': process.env.CHIRP_UPLOADS_URL ?? 'http://localhost:8084',
  '/api/follows': process.env.CHIRP_FOLLOWS_URL ?? 'http://localhost:8083',
}

export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: proxyTargets,
  },
})
