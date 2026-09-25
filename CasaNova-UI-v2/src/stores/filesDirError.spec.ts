// Focused tests for how a directory LOAD FAILURE reaches the file browser.
//
// Regression context: FileBrowserView drew "This folder is empty. Drag files
// here to upload" for every outcome — denied, 500, network down, expired
// token — because fetchDir swallowed the error (`if (result.error) return`)
// and the view derived emptiness from `visibleEntries.length === 0` alone.
// A refusal was presented as a result, and the empty state then invited an
// upload into a path that had just refused to list.
//
// These tests pin the store contract the view's branch order depends on:
//   - a failed load records the BACKEND'S OWN message for that path;
//   - a path that has never been loaded is not reported as loaded (so the
//     view must not call it empty);
//   - a directory that really is empty still reads as loaded-and-empty —
//     the control that stops this fix turning every legitimate empty
//     folder into a false error;
//   - a later success clears the error, and errors are tracked per path so
//     one bad directory does not poison its neighbour.

import { describe, it, expect, beforeEach, vi } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'

vi.mock('@/api/safeCall', () => ({
  safeCall: async (fn: () => Promise<unknown>) => {
    try {
      return { data: await fn(), fixture: false, error: null, status: null }
    } catch (e: unknown) {
      return {
        data: undefined,
        fixture: false,
        error: e instanceof Error ? e.message : String(e),
        status: null,
      }
    }
  },
}))

vi.mock('@/api', () => ({
  filesApi: { listDir: vi.fn() },
}))

vi.mock('@/composables/syncedPrefs', async () => {
  const { ref } = await import('vue')
  const generation = ref(0)
  return {
    getPref: () => null,
    setPref: () => {},
    // prefsGeneration is a FUNCTION returning the ref the store watches.
    prefsGeneration: () => generation,
  }
})

import { filesApi } from '@/api'
import { useFilesStore } from './files'

// The store registers a window 'auth-logout' listener at setup time. Provide
// a no-op window so it can be exercised under Vitest's default node
// environment without pulling in jsdom — same stub as containers.spec.ts.
vi.stubGlobal('window', {
  addEventListener: () => {},
  removeEventListener: () => {},
})

const listDirMock = () => filesApi.listDir as unknown as ReturnType<typeof vi.fn>

// cd() starts the fetch and returns synchronously (`void fetchDir(path)`) —
// that is how the view calls it. Drain the queue so the store is left in the
// state the view would actually render.
const goTo = async (store: ReturnType<typeof useFilesStore>, path: string) => {
  store.cd(path)
  await new Promise((resolve) => setTimeout(resolve, 0))
}

const entry = (name: string) => ({
  name,
  path: '/data/' + name,
  is_dir: false,
  size: 1,
  modified: '2026-01-01T00:00:00Z',
})

// The exact refusal the backend returns today for a denied path, which is
// the message the user must end up seeing instead of "this folder is empty".
const DENIED = 'access to /etc is not allowed'

describe('files store — a directory that could not be read is not an empty one', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
  })

  it('records the backend message for a path whose load failed', async () => {
    listDirMock().mockRejectedValue(new Error(DENIED))
    const store = useFilesStore()
    await goTo(store, '/etc')
    expect(store.currentDirError).toBe(DENIED) // the WHY survives to the view
    expect(store.currentDirLoaded).toBe(false)
    expect(store.visibleEntries).toHaveLength(0) // ...but emptiness is not the story
  })

  it('does not report a never-loaded directory as loaded', () => {
    const store = useFilesStore()
    expect(store.currentDirLoaded).toBe(false)
    expect(store.currentDirError).toBeNull() // absence of data is not an error either
  })

  it('CONTROL: a directory that really is empty still reads as loaded and error-free', async () => {
    listDirMock().mockResolvedValue([])
    const store = useFilesStore()
    await goTo(store, '/data/empty')
    expect(store.currentDirError).toBeNull()
    expect(store.currentDirLoaded).toBe(true) // loaded...
    expect(store.visibleEntries).toHaveLength(0) // ...and genuinely empty
  })

  it('shows a successful listing with no error', async () => {
    listDirMock().mockResolvedValue([entry('a.txt'), entry('b.txt')])
    const store = useFilesStore()
    await goTo(store, '/data')
    expect(store.currentDirError).toBeNull()
    expect(store.currentDirLoaded).toBe(true)
    expect(store.visibleEntries).toHaveLength(2)
  })

  it('clears the error once the same path loads successfully', async () => {
    const store = useFilesStore()
    listDirMock().mockRejectedValue(new Error(DENIED))
    await goTo(store, '/data')
    expect(store.currentDirError).toBe(DENIED)

    listDirMock().mockResolvedValue([entry('a.txt')])
    await store.retryCurrentDir()
    expect(store.currentDirError).toBeNull()
    expect(store.currentDirLoaded).toBe(true)
    expect(store.visibleEntries).toHaveLength(1)
  })

  it('keeps errors per path, so one unreadable directory does not poison another', async () => {
    const store = useFilesStore()
    listDirMock().mockRejectedValue(new Error(DENIED))
    await goTo(store, '/etc')
    expect(store.currentDirError).toBe(DENIED)

    listDirMock().mockResolvedValue([entry('a.txt')])
    await goTo(store, '/data')
    expect(store.currentDirError).toBeNull() // the good directory is clean

    listDirMock().mockRejectedValue(new Error(DENIED))
    await goTo(store, '/etc')
    expect(store.currentDirError).toBe(DENIED) // and the bad one still remembers
  })
})
