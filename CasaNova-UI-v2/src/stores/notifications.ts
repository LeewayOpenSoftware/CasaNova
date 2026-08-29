// Notifications store.
//
// Owns the list + unread count + sidebar open/close state. The sidebar
// component reads the list via the store rather than calling the API
// directly so a single poll drives every open instance of the sidebar
// (future-proofing for when we also show a count badge in the top bar).
//
// Polling strategy: fetch every 30 seconds while the user is active.
// When Wave 2 / MessageBus Socket.IO subscription lands, this store will
// switch to push-based updates and the interval goes away. The API
// surface stays identical — callers only see `list`, `unreadCount`, and
// the mutation methods.

import { defineStore } from 'pinia'
import { ref, computed, watch, onScopeDispose } from 'vue'
import type { Notification, NotificationCategory } from '@/api/notifications'
import * as api from '@/api/notifications'
// Notifications start empty when backend is unreachable.

// 30 second poll — fast enough that a newly-emitted notification feels
// live, slow enough that idle tabs don't hammer the backend.
const POLL_INTERVAL_MS = 30_000

// Persistent dismissed/read state stored in localStorage so the user's
// dismiss/mark-read actions survive page reloads.
const DISMISSED_KEY = 'casanova.notifications.dismissed'
const READ_KEY = 'casanova.notifications.read'
// Cap each persisted Set so a long-running session doesn't grow the
// localStorage entry without bound. The newest IDs are kept; older ones
// are pruned. 2 000 IDs is a couple of weeks of typical notification
// volume and still encodes to <30 KB.
const ID_SET_CAP = 2000

function loadIdSet(key: string): Set<number> {
  if (typeof window === 'undefined') return new Set()
  try {
    const raw = window.localStorage.getItem(key)
    if (!raw) return new Set()
    const parsed = JSON.parse(raw)
    if (!Array.isArray(parsed)) return new Set()
    return new Set(parsed.filter((v) => typeof v === 'number'))
  } catch {
    return new Set()
  }
}
function saveIdSet(key: string, set: Set<number>) {
  try {
    let arr = [...set]
    if (arr.length > ID_SET_CAP) {
      // Keep the largest (= most recent) IDs. Notification IDs are
      // monotonic from the backend, so a simple numeric tail-window
      // beats any LRU bookkeeping.
      arr.sort((a, b) => a - b)
      arr = arr.slice(arr.length - ID_SET_CAP)
      // Trim the in-memory Set too so subsequent saves don't keep
      // re-pruning the same long array.
      set.clear()
      for (const v of arr) set.add(v)
    }
    window.localStorage.setItem(key, JSON.stringify(arr))
  } catch {
    /* sandboxed — ignore */
  }
}

export const useNotificationsStore = defineStore('notifications', () => {
  const items = ref<Notification[]>([])
  const unreadCount = ref(0)
  const sidebarOpen = ref(false)
  const loading = ref(false)
  const lastError = ref<string | null>(null)
  // Kept for interface compatibility; always false now that fixtures
  // have been removed.
  const fixtureMode = ref(false)

  // Active category filter. `null` = "All". Drives both the store's
  // visible list (via filteredItems) and the tab UI in the drawer.
  const categoryFilter = ref<NotificationCategory | null>(null)

  // Persistent id sets so the user's dismiss/mark-read actions
  // survive page reloads between poll ticks.
  const dismissedIds = ref<Set<number>>(loadIdSet(DISMISSED_KEY))
  const readIds = ref<Set<number>>(loadIdSet(READ_KEY))

  // The Sets are replaced (not mutated in-place) on every change,
  // so shallow watch suffices — deep: true would needlessly iterate the Set.
  watch(dismissedIds, (v) => saveIdSet(DISMISSED_KEY, v))
  watch(readIds, (v) => saveIdSet(READ_KEY, v))

  let pollTimer: number | null = null  // defer timeout OR interval handle
  let pollInterval: number | null = null
  let refreshInFlight = false

  // Wave 31: session epoch — bumped on every stopPolling() so an
  // in-flight refresh() that returns AFTER a logout cannot leak the
  // previous user's notifications back into items.value. Without this
  // guard, the sequence:
  //   1. user A: refresh() starts, server returns 200
  //   2. user A: logs out before the response is parsed
  //   3. auth-logout fires → stopPolling() runs, but items.value not touched
  //   4. promise resolves → items.value = userA.notifications
  //   5. user B logs in → sees user A's notifications until next poll
  // ...left private notifications visible across sessions on the same tab.
  let sessionEpoch = 0

  const unreadItems = computed(() => items.value.filter((n) => !n.read))

  // filteredItems — what the drawer actually renders. Applies the
  // current category filter on top of the raw list.
  const filteredItems = computed(() =>
    categoryFilter.value === null
      ? items.value
      : items.value.filter((n) => n.category === categoryFilter.value),
  )

  function setCategoryFilter(c: NotificationCategory | null) {
    categoryFilter.value = c
  }

  async function refresh() {
    if (refreshInFlight) return
    refreshInFlight = true
    loading.value = true
    lastError.value = null
    const startedAt = sessionEpoch
    try {
      const result = await api.list({ limit: 50 })
      // Wave 31: discard the response if the session was torn down
      // (logout / user switch) while we were waiting on the network.
      if (startedAt !== sessionEpoch) return
      items.value = result.notifications ?? []
      unreadCount.value = result.unread_count ?? 0
      fixtureMode.value = false
    } catch (err) {
      if (startedAt !== sessionEpoch) return
      // Backend unreachable — show empty state instead of fake notifications.
      items.value = []
      unreadCount.value = 0
      fixtureMode.value = false
      lastError.value = err instanceof Error ? err.message : 'Failed to load notifications'
    } finally {
      loading.value = false
      refreshInFlight = false
    }
  }

  // startPolling is idempotent — calling it twice doesn't create two
  // timers. The layout component mounts once per session so this is
  // defensive but cheap. Skips polls when the tab is hidden so
  // background tabs don't hammer the backend.
  function startPolling() {
    if (pollTimer !== null || pollInterval !== null) return
    // Defer the first fetch by 3 seconds so it doesn't compete with the
    // critical container + system utilization requests on page reload.
    // The notification badge updates after a brief delay — imperceptible
    // to the user, but lets the dashboard render noticeably faster.
    pollTimer = window.setTimeout(() => {
      pollTimer = null
      void refresh()
      // Guard against re-entry: only create the interval if another
      // startPolling() call hasn't already set one up while we waited.
      if (pollInterval === null) {
        pollInterval = window.setInterval(() => {
          if (typeof document !== 'undefined' && document.hidden) return
          void refresh()
        }, POLL_INTERVAL_MS)
      }
    }, 3000)
  }

  function stopPolling() {
    if (pollTimer !== null) {
      window.clearTimeout(pollTimer)
      pollTimer = null
    }
    if (pollInterval !== null) {
      window.clearInterval(pollInterval)
      pollInterval = null
    }
    // Wave 31: bump the epoch and clear local state so any in-flight
    // refresh() resolves into a no-op and the next user starts with
    // an empty list. See sessionEpoch comment above.
    sessionEpoch++
    items.value = []
    unreadCount.value = 0
    lastError.value = null
  }

  function toggleSidebar() {
    sidebarOpen.value = !sidebarOpen.value
    // Refresh on open so the user sees the freshest state the moment
    // they click the bell — don't wait for the next poll tick.
    if (sidebarOpen.value) void refresh()
  }

  function closeSidebar() {
    sidebarOpen.value = false
  }

  async function markOneRead(id: number) {
    // In fixture mode, skip the API call and mutate locally so the dev
    // scaffold feels real without a backend. Production mode hits the
    // API first so server and client stay consistent on failure.
    if (!fixtureMode.value) {
      try {
        await api.markRead({ ids: [id] })
      } catch (err) {
        console.error('Failed to mark notification as read:', err)
      }
    }
    // Immutable update so computeds (unreadItems, filteredItems) recompute.
    const target = items.value.find((n) => n.id === id)
    if (target && !target.read) {
      items.value = items.value.map((n) => (n.id === id ? { ...n, read: true } : n))
      unreadCount.value = Math.max(0, unreadCount.value - 1)
    }
    // Persist so the next fixture refresh doesn't mark it unread again.
    if (fixtureMode.value) {
      const next = new Set(readIds.value)
      next.add(id)
      readIds.value = next
    }
  }

  /**
   * dismissAll — clears every notification in one go. Notifications
   * aren't email; there's no need for a read/unread toggle when you
   * just want them gone. Persists all current ids to `dismissedIds`
   * so the 30 s poll never resurfaces them in fixture mode.
   */
  async function dismissAll() {
    if (!fixtureMode.value) {
      // Real backend: try to mark all as read first, then delete each.
      // allSettled ensures one failed delete doesn't abort the rest.
      try {
        await api.markRead({ all: true })
      } catch (err) {
        console.error('Failed to mark all notifications as read:', err)
      }
      await Promise.allSettled(items.value.map((n) => api.remove(n.id)))
    }
    // Persist dismissal ids so future refreshes don't re-surface them.
    const next = new Set(dismissedIds.value)
    for (const n of items.value) next.add(n.id)
    dismissedIds.value = next
    items.value = []
    unreadCount.value = 0
  }

  async function removeOne(id: number) {
    if (!fixtureMode.value) {
      try {
        await api.remove(id)
      } catch (err) {
        console.error('Failed to remove notification:', err)
      }
    }
    // Immutable removal — splice mutates in-place which can miss watchers.
    const target = items.value.find((n) => n.id === id)
    if (target) {
      const wasUnread = !target.read
      items.value = items.value.filter((n) => n.id !== id)
      if (wasUnread) unreadCount.value = Math.max(0, unreadCount.value - 1)
    }
    // Persist so the next fixture refresh doesn't resurface it.
    if (fixtureMode.value) {
      const next = new Set(dismissedIds.value)
      next.add(id)
      dismissedIds.value = next
    }
  }

  // Stop polling when the user logs out so stale-token API calls don't
  // fire after the session is cleared.
  function _onAuthLogout() { stopPolling() }
  window.addEventListener('auth-logout', _onAuthLogout)

  onScopeDispose(() => {
    stopPolling()
    window.removeEventListener('auth-logout', _onAuthLogout)
  })

  return {
    items,
    unreadItems,
    filteredItems,
    unreadCount,
    sidebarOpen,
    loading,
    lastError,
    fixtureMode,
    categoryFilter,
    refresh,
    startPolling,
    stopPolling,
    toggleSidebar,
    closeSidebar,
    markOneRead,
    dismissAll,
    removeOne,
    setCategoryFilter,
  }
})
