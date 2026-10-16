/// <reference types="vitest/config" />
// The hub's web UI (docs/ui.md). `npm run build` writes the bundle to
// hub/ui/static/app, which hub/ui embeds in the hub binary.
//
// `npm run dev` serves the UI with hot reload and proxies the hub's API
// to a local hub (SILO_HUB, http://localhost:8080 by default) started
// with -dev-user-header: the proxy adds that header, as SILO_DEV_USER
// (dev@example.com by default), to every request, the event streams
// included, since a browser's EventSource cannot set a header itself.
// The built app never sets it: behind IAP the browser's own cookie
// signs the caller in.
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

const hub = process.env.SILO_HUB ?? 'http://localhost:8080';
const header = process.env.SILO_DEV_USER_HEADER ?? 'X-Silo-User';
const user = process.env.SILO_DEV_USER ?? 'dev@example.com';

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../hub/ui/static/app',
    emptyOutDir: true,
    assetsDir: 'assets',
    chunkSizeWarningLimit: 1024,
  },
  server: {
    proxy: {
      '/v1': { target: hub, changeOrigin: true, headers: { [header]: user } },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['src/test/setup.ts'],
  },
});
