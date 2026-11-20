import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// API_URL points the dev server at another API, e.g. API_URL=http://localhost:8081 npm run dev
export default defineConfig({
  plugins: [react()],
  // xfwd: the API sees who called (X-Forwarded-For), for the joiner page's rate limits.
  server: { port: 5173, proxy: { '/api': { target: process.env.API_URL || 'http://localhost:8080', changeOrigin: true, xfwd: true } } },
});
