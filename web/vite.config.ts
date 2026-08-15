/// <reference types="vitest/config" />
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// ─────────────────────────────────────────────────────────────────────────────
// Go embed contract (P1.5 → later Go embed task)
// ─────────────────────────────────────────────────────────────────────────────
// `npm run build` emits web/dist. The Go daemon serves this via `//go:embed dist`
// (an embed.FS) behind a static file handler with SPA fallback to index.html.
//
//   base: './'  ⇒ ALL asset URLs in index.html are relative (./assets/...), so the
//   bundle serves correctly from ANY mount path the Go handler chooses (root '/',
//   or under a sub-path, or behind Tailscale Serve which may add a path prefix).
//   Do NOT change base to an absolute path — it breaks embed serving under a prefix.
//
// Everything is self-contained: no external CDN/font/script references land in
// dist/index.html (verified in the build gate by grepping for http(s):// hosts).
// The phone loads this over Tailscale with no outbound internet.
// ─────────────────────────────────────────────────────────────────────────────

export default defineConfig({
  base: './',
  plugins: [react()],
  build: {
    outDir: 'dist',
    // Emit relative, hashed assets; keep it small (phone over Tailscale).
    assetsDir: 'assets',
    sourcemap: false,
    target: 'es2020',
    // Single small app: default rollup chunking is fine; avoid vendor splitting
    // that would add more requests over a phone link.
  },
  server: {
    // Dev-only proxy → the daemon's default bind (configs/config.example.yaml:
    // server.port = 39876, bind 127.0.0.1). In production the Go daemon serves
    // both the API and these static assets from the same origin, so no proxy runs.
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:39876',
        changeOrigin: true,
        // The live channel's default transport is a WebSocket at /api/ws (live/connection.ts
        // defaultWsUrl), which matches THIS rule, not the '/ws' rule below. Without ws:true the
        // upgrade is proxied as plain HTTP, the socket never opens, and `npm run dev` sits in a
        // reconnect loop with the GPU panel stuck on "Waiting for first hardware frame…".
        // Production is unaffected (the Go daemon serves API + assets from one origin).
        ws: true,
      },
      '/ws': {
        target: 'ws://127.0.0.1:39876',
        ws: true,
        changeOrigin: true,
      },
    },
  },
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    css: false,
  },
});
