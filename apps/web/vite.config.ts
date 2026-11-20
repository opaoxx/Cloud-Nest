import { defineConfig, loadEnv } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, '.', ['VITE_CLOUDNEST_DATA_MODE', 'CLOUDNEST_WEB_API_TARGET'])
  return {
    plugins: [react()],
    server: env.VITE_CLOUDNEST_DATA_MODE === 'api' ? {
      proxy: {
        '/api': {
          target: env.CLOUDNEST_WEB_API_TARGET || 'http://127.0.0.1:18081',
          changeOrigin: true,
        },
      },
    } : undefined,
  }
})
