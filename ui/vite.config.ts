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
// signs the caller in. The proxy also gives each request the hub's own
// Origin, since the hub accepts a write only from its own origin
// (identity.SameOrigin) and the browser's is the dev server's.
//
// The UI reaches the API only through the client SDK, sdk/ts, which it
// builds from source as @silo/hub-sdk (tsconfig.json's paths say the
// same to tsc).
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

const hub = process.env.SILO_HUB ?? 'http://localhost:8080';
const header = process.env.SILO_DEV_USER_HEADER ?? 'X-Silo-User';
const user = process.env.SILO_DEV_USER ?? 'dev@example.com';

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { '@silo/hub-sdk': fileURLToPath(new URL('../sdk/ts/src/index.ts', import.meta.url)) },
  },
  build: {
    outDir: '../hub/ui/static/app',
    emptyOutDir: true,
    assetsDir: 'assets',
    chunkSizeWarningLimit: 1024,
  },
  server: {
    fs: { allow: ['.', '../sdk/ts/src'] },
    proxy: {
      '/v1': { target: hub, changeOrigin: true, headers: { [header]: user, Origin: new URL(hub).origin } },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    testTimeout: 30000,
    hookTimeout: 30000,
    setupFiles: ['src/test/setup.ts'],
    // e2e/ is Playwright's (playwright.config.ts), not vitest's.
    include: ['src/**/*.test.{ts,tsx}'],
  },
});
