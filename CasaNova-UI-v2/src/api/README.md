# CasaNova API client layer

This directory is the **single integration point** between the Vue 3
frontend and the CasaNova Go services. Every store that needs live
data imports from `@/api` and wraps each call in `safeCall()`.

## Architecture

```
 Components  →  Pinia stores  →  safeCall(apiFn, fixtureFn)
                                     │
                                     ├── real backend (when up)
                                     │    via axios client + Vite proxy
                                     │
                                     └── fixture fallback (dev only)
                                          when backend unreachable
```

## Files

| File                  | Service target                       | Notes |
|----------------------|---------------------------------------|-------|
| `client.ts`          | —                                    | Axios instance + auth interceptor |
| `safeCall.ts`        | —                                    | Unified try/catch + fixture fallback helper |
| `system.ts`          | `CasaNova` (main)                    | `/v1/sys/*` — utilization, audit, version, diagnosis, power, SSH |
| `files.ts`           | `CasaNova` (main)                    | `/v1/folder`, `/v1/file`, `/v1/file/batch`, `/v1/trash`, `/v1/samba` |
| `storage.ts`         | `CasaNova-LocalStorage`              | `/v2/local_storage/*` — disks, SMART, mounts, merge, cloud |
| `containers.ts`      | `CasaNova-AppManagement`             | `/v2/app_management/compose/*` — list, install, lifecycle, SSE progress |
| `notifications.ts`   | `CasaNova-Common` (via main)         | `/v1/sys/notifications` |
| `users.ts`           | `CasaNova-UserService`               | `/v1/users/*` |
| `index.ts`           | —                                    | Barrel re-export |

## Strict mode vs dev fallback

`safeCall` reads `import.meta.env.VITE_STRICT_API`. When set to `'1'`
the fallback factory is skipped and every failure bubbles as an error.
**Production builds MUST set this**. Dev builds leave it off so the
UI scaffold is usable without a running backend.

```bash
# Dev — fallbacks enabled (default)
npm run dev

# Strict — real API only, failures surface to the user
VITE_STRICT_API=1 npm run dev

# Build for production — strict is implicit via .env.production
npm run build
```

## Store → API wiring status

| Store                      | Wired via safeCall? | Fixture source |
|---------------------------|---------------------|----------------|
| `auth`                    | yes (login only)    | `users.ts` has a dev-only fallback token |
| `notifications`           | yes                 | `fixtures/notifications.ts` |
| `system`                  | yes                 | `fixtures/dashboard.ts` (drifts) |
| `installProgress`         | fixture-only        | in-memory timer. Swap point: `beginInstall()` |
| `files`                   | partial (upload)    | local reactive Map + blob URLs. Swap: `listDir`, `mkdir`, `renameEntry`, `deleteEntries` |
| `theme`                   | client-only         | localStorage |
| `users`                   | fixture-only        | `fixtures/users.ts`. Swap: when `/v1/users` CRUD endpoints land |
| `dashboardPrefs`          | client-only         | localStorage |

Partial/fixture-only stores are **intentional** — they're either
client-state-only (theme, dashboardPrefs) or waiting on a backend
swap that's scheduled for a later integration pass (`files` listDir,
`installProgress` SSE). Every one has the swap point commented at
the method level so grep for `Real backend` or `swap` to find them.

## Adding a new endpoint

1. Add the typed wrapper to the matching module (`system.ts`,
   `files.ts`, etc). Return the unwrapped data (`res.data.data` in
   most cases since CasaNova envelops responses in
   `{ success: 200, data: T }`).
2. Add a fixture factory at the fixture level or in the store's
   `refresh()` if the fallback is dynamic.
3. Wrap the call site in `safeCall(api, fixture)` and handle the
   three branches: `result.data`, `result.fixture`, `result.error`.
4. Write-through mutations should check `fixtureMode` on the store
   and skip the API call when it's true (mutate locally instead).

## Backend reachability

`safeCall` falls back on:
- Network errors (`ERR_NETWORK`, `Network Error`)
- Vite proxy 5xx (backend unreachable surfaces as 502/504)
- Raw 5xx from the upstream

4xx errors (bad request, unauthorized, not found) **never** fall
back — those are real API mistakes and the caller needs to see them.
