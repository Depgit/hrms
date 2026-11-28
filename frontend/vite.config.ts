import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// API_URL points the dev server at another API, e.g. API_URL=http://localhost:8081 npm run dev
// The web runs on port 3001 (owner, 28 Nov 2026; 3000 is the Purchase Desk); WEB_PORT=5191 npm run dev picks another.
export default defineConfig({
  plugins: [react()],
  // xfwd: the API sees who called (X-Forwarded-For), for the joiner page's rate limits.
  server: { port: Number(process.env.WEB_PORT) || 3001, strictPort: true, proxy: { '/api': { target: process.env.API_URL || 'http://localhost:8080', changeOrigin: true, xfwd: true } } },
});
