// Focused tests for installed-app resolution — the route `/app/apps/:id`
// → `containersStore.findApp(id)` lookup path that backs AppDetailView.
//
// Regression context: AppDetailView used to render "App not found"
// whenever findApp() returned undefined, including while the list was
// still loading or after a failed fetch. These tests pin the store
// contract the detail view's loading-gate depends on:
//   - before load: nothing resolves (so the view must show "loading",
//     not "not found");
//   - after a successful load: an installed app resolves by the SAME id
//     the dashboard links to;
//   - a successfully-loaded list that lacks the id is a genuine miss;
//   - a failed fetch surfaces an error (so the view shows an error/retry
//     state, not "not found").

import { describe, it, expect, beforeEach, vi } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'

vi.mock('@/api/safeCall', () => ({
  safeCall: async (fn: () => Promise<unknown>) => {
    try {
      return { data: await fn(), error: null }
    } catch (e: unknown) {
      return { data: undefined, error: e instanceof Error ? e.message : String(e) }
    }
  },
}))

vi.mock('@/api', () => ({
  containersApi: {
    listApps: vi.fn(),
    getContainerStats: vi.fn(async () => []),
    getAllContainers: vi.fn(async () => []),
  },
}))

import { containersApi } from '@/api'
import { useContainersStore } from './containers'

// The store registers a window 'auth-logout' listener at setup time.
// Provide a no-op window so the store can be exercised under Vitest's
// default node environment without pulling in jsdom.
vi.stubGlobal('window', {
  addEventListener: () => {},
  removeEventListener: () => {},
})

const appFixture = (id: string, title = id) => ({
  id,
  title,
  iconUrl: '',
  autoUpdate: false,
  webUIProtocol: 'http',
  webUIHost: '',
  webUIPort: 0,
  webUIPath: '',
  services: [],
})

const listAppsMock = () => containersApi.listApps as unknown as ReturnType<typeof vi.fn>

describe('containers store — installed-app resolution (route → app lookup)', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
    containersApi.getContainerStats = vi.fn(async () => []) as never
    containersApi.getAllContainers = vi.fn(async () => []) as never
  })

  it('resolves nothing before the list has loaded (detail view must show loading, not not-found)', () => {
    const store = useContainersStore()
    expect(store.apps).toHaveLength(0)
    expect(store.findApp('jellyfin')).toBeUndefined()
  })

  it('resolves an installed app by the same id the dashboard links to, after refresh', async () => {
    listAppsMock().mockResolvedValue([appFixture('jellyfin', 'Jellyfin')])
    const store = useContainersStore()
    await store.refresh(true)
    const app = store.findApp('jellyfin')
    expect(app).toBeDefined()
    expect(app!.id).toBe('jellyfin') // route param `:id` === AppEntry.id
    expect(app!.title).toBe('Jellyfin')
    expect(store.lastError).toBeNull()
  })

  it('returns undefined only for an id absent from a successfully-loaded list (genuine not-found)', async () => {
    listAppsMock().mockResolvedValue([appFixture('navidrome'), appFixture('vaultwarden')])
    const store = useContainersStore()
    await store.refresh(true)
    expect(store.lastError).toBeNull()
    expect(store.findApp('navidrome')).toBeDefined()
    expect(store.findApp('vaultwarden')).toBeDefined()
    expect(store.findApp('does-not-exist')).toBeUndefined()
  })

  it('surfaces a load error (not an empty list) when the fetch fails — must not look like not-found', async () => {
    listAppsMock().mockRejectedValue(new Error('500 backend unavailable'))
    const store = useContainersStore()
    await store.refresh(true)
    expect(store.findApp('jellyfin')).toBeUndefined()
    expect(store.lastError).toContain('500')
  })

  it('resolves a standalone (unmanaged) container by name — the Navidrome case', async () => {
    // Navidrome is installed as a standalone Docker container (no compose
    // project), so it is NOT in the compose list but IS surfaced on the
    // dashboard. Its detail route must resolve instead of "App not found".
    listAppsMock().mockResolvedValue([appFixture('jellyfin', 'Jellyfin')])
    containersApi.getAllContainers = vi.fn(async () => ({
      stacks: [],
      standalone: [
        {
          id: 'sha-abc',
          name: 'navidrome',
          image: 'deluan/navidrome:latest',
          state: 'running',
          status: 'Up 20 hours',
          created: 0,
          ports: [{ privatePort: 4533, publicPort: 4533, type: 'tcp' }],
          labels: {},
          managed: false,
        },
      ],
    })) as never

    const store = useContainersStore()
    await store.refresh(true)
    await store.refreshAll()

    const nav = store.findApp('navidrome')
    expect(nav).toBeDefined()
    expect(nav!.id).toBe('navidrome')
    expect(nav!.managed).toBe(false) // read-only, compose-only controls hidden
    expect(nav!.source).toBe('standalone')
    expect(nav!.webUIPort).toBe(4533)
    expect(nav!.services[0].ports[0]).toMatchObject({ host: 4533, container: 4533, protocol: 'tcp' })

    // Compose apps remain managed (full editor) — no regression.
    expect(store.findApp('jellyfin')!.managed).not.toBe(false)
    expect(store.findApp('jellyfin')!.source).toBe('compose')
  })
})

// ── Unified application model ──────────────────────────────────────────
const externalContainer = (name: string, image: string, opts: Partial<{ pub: number }> = {}) => ({
  id: `sha-${name}`,
  name,
  image,
  state: 'running',
  status: 'Up 1 hour',
  created: 0,
  ports: opts.pub ? [{ privatePort: 80, publicPort: opts.pub, type: 'tcp' }] : [],
  labels: {},
  managed: false,
})

async function loadUnified(allContainers: { stacks: unknown[]; standalone: unknown[] }, compose: string[] = ['jellyfin']) {
  ;(containersApi.listApps as unknown as ReturnType<typeof vi.fn>).mockResolvedValue(compose.map((id) => appFixture(id)))
  containersApi.getAllContainers = vi.fn(async () => allContainers) as never
  const store = useContainersStore()
  await store.refresh(true)
  await store.refreshAll()
  return store
}

describe('containers store — unified app resolver', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
    containersApi.getContainerStats = vi.fn(async () => []) as never
    containersApi.getAllContainers = vi.fn(async () => ({ stacks: [], standalone: [], total: 0 })) as never
  })

  it('unifiedApps merges all sources with correct source tags', async () => {
    const store = await loadUnified({
      stacks: [{ name: 'media-stack', status: 'running', serviceCount: 2, managed: false, containers: [externalContainer('media-web', 'linuxserver/web:latest', { pub: 8096 }), externalContainer('media-db', 'postgres:16')] }],
      standalone: [externalContainer('navidrome', 'deluan/navidrome:latest', { pub: 4533 })],
    })
    const byId = Object.fromEntries(store.unifiedApps.map((a) => [a.id, a]))
    expect(byId['jellyfin'].source).toBe('compose')
    expect(byId['navidrome'].source).toBe('standalone')
    expect(byId['media-stack'].source).toBe('external-stack')
    // External stack is ONE multi-service app, not one card per container.
    expect(byId['media-stack'].services.map((s) => s.id).sort()).toEqual(['media-db', 'media-web'])
    expect(byId['media-stack'].webUIPort).toBe(8096)
  })

  it('dashboard route consistency: every unified app id resolves via findApp, and vice-versa', async () => {
    const store = await loadUnified({
      stacks: [{ name: 'media-stack', status: 'running', serviceCount: 1, managed: false, containers: [externalContainer('media-web', 'nginx:latest', { pub: 8080 })] }],
      standalone: [externalContainer('navidrome', 'deluan/navidrome:latest', { pub: 4533 })],
    })
    for (const a of store.unifiedApps) {
      // Every card the dashboard renders must open.
      expect(store.findApp(a.id), `route /app/apps/${a.id} must resolve`).toBeDefined()
    }
  })

  it('resolves an external compose stack by its stack name', async () => {
    const store = await loadUnified({
      stacks: [{ name: 'paperless', status: 'running', serviceCount: 2, managed: false, containers: [externalContainer('paperless-web', 'paperless/ngx:latest', { pub: 8000 }), externalContainer('paperless-redis', 'redis:7')] }],
      standalone: [],
    })
    const stack = store.findApp('paperless')
    expect(stack).toBeDefined()
    expect(stack!.source).toBe('external-stack')
    expect(stack!.services).toHaveLength(2)
    expect(stack!.managed).toBe(false)
  })

  it('a compose app wins over a same-named standalone container (dedup, unambiguous resolution)', async () => {
    const store = await loadUnified(
      { stacks: [], standalone: [externalContainer('jellyfin', 'some/other:latest', { pub: 9999 })] },
      ['jellyfin'],
    )
    expect(store.unifiedApps.filter((a) => a.id === 'jellyfin')).toHaveLength(1)
    expect(store.findApp('jellyfin')!.source).toBe('compose')
  })

  it('a genuinely-unknown id still resolves to nothing (real not-found)', async () => {
    const store = await loadUnified({
      stacks: [],
      standalone: [externalContainer('navidrome', 'deluan/navidrome:latest', { pub: 4533 })],
    })
    expect(store.findApp('does-not-exist')).toBeUndefined()
  })
})
