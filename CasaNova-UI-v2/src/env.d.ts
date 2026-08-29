/// <reference types="vite/client" />

// Ambient type declarations for non-TS imports.

// Vue SFC: allow `import Foo from './Foo.vue'` in .ts files.
declare module '*.vue' {
  import type { DefineComponent } from 'vue'
  const component: DefineComponent<object, object, unknown>
  export default component
}

// JSON locale files — typed as a generic record; the actual type safety
// comes from the `MessageSchema` derived from en_US in src/i18n/index.ts.
declare module '*.json' {
  const value: Record<string, unknown>
  export default value
}

interface ImportMetaEnv {
  readonly VITE_BACKEND_URL?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
