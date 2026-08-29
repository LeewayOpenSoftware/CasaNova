import { defineConfig, loadEnv } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

// Vite config for CasaNova-UI v2.
//
// Dev proxy points every /v1, /v2, /v3 request at the running CasaNova backend.
// The backend's host+port come from .env.local (gitignored) so each developer
// can point at their own test instance without touching committed config.
//
// Build output lands in the path the CasaNova packaging expects
// (`build/sysroot/var/lib/casanova/www/`) so the resulting bundle slots into
// the existing release scripts without adjustment.
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const backend = env.VITE_BACKEND_URL || 'http://localhost:81'

  return {
    plugins: [vue()],
    resolve: {
      alias: {
        '@': fileURLToPath(new URL('./src', import.meta.url)),
      },
    },
    server: {
      // Honor PORT env var when the preview harness injects one, fall back
      // to the default 5173 for local dev runs.
      port: process.env.PORT ? Number(process.env.PORT) : 5173,
      proxy: {
        '/v1': { target: backend, changeOrigin: true },
        '/v2': { target: backend, changeOrigin: true, ws: true },
        '/v3': { target: backend, changeOrigin: true },
        '/.well-known': { target: backend, changeOrigin: true },
      },
    },
    // @novnc/novnc uses top-level await; vite's dep pre-bundler (esbuild)
    // chokes on it and 504s the dep in dev. Serve it un-bundled as native ESM,
    // which handles TLA fine. No effect on the production build (rollup bundles
    // it — see build.target below).
    optimizeDeps: {
      exclude: ['@novnc/novnc'],
    },
    build: {
      outDir: 'build/sysroot/var/lib/casanova/www',
      emptyOutDir: true,
      sourcemap: true,
      // es2022 so top-level await (used by @novnc/novnc 1.7) compiles. Every
      // browser CasaNova targets (2023+) supports it.
      target: 'es2022',
    },
  }
})
