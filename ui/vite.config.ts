import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

const basePath = process.env.BASE_PATH || '/'

export default defineConfig({
  base: basePath,
  plugins: [react()],
  server: {
    proxy: {
      '/api': 'http://localhost:8000',
      '/v2': 'http://localhost:8000',
      '/health': 'http://localhost:8000',
      '/metrics': 'http://localhost:8000',
    },
  },
  define: {
    '__BASE_PATH__': JSON.stringify(basePath),
  },
})
