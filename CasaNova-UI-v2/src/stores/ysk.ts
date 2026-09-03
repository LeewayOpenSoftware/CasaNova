// YSK card store — caches the active card list and refreshes on bus
// pushes (ysk:card:upsert / ysk:card:delete) so dismissing a card on
// one tab updates every other tab in the same window without a manual
// reload.
//
// Polling is the fallback when the WebSocket isn't connected (e.g.
// during a backend restart). Default cadence is 60s — cards are
// long-lived hints, not real-time signals.

import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { listYskCards, dismissYskCard, type YSKCard } from '@/api/ysk'
import { safeCall } from '@/api/safeCall'

const POLL_INTERVAL_MS = 60_000

export const useYskStore = defineStore('ysk', () => {
  const cards = ref<YSKCard[]>([])
  const loading = ref(false)
  const lastError = ref<string | null>(null)
  const lastFetchedAt = ref<number>(0)

  // Local dismissal stash. We optimistically remove a card from the
  // list the moment the user clicks dismiss so the UI feels instant;
  // the network round trip catches up in the background, and we
  // reinstate the card only if the DELETE call fails.
  const pendingDismiss = ref<Set<string>>(new Set())

  let pollTimer: ReturnType<typeof setInterval> | null = null

  const visibleCards = computed<YSKCard[]>(() =>
    cards.value.filter((c) => !pendingDismiss.value.has(c.id)),
  )

  async function fetchCards(): Promise<void> {
    if (loading.value) return
    loading.value = true
    const result = await safeCall(() => listYskCards())
    loading.value = false
    if (result.error) {
      lastError.value = result.error
      return
    }
    if (result.data) {
      cards.value = result.data
      lastError.value = null
      lastFetchedAt.value = Date.now()
    }
  }

  async function dismiss(id: string): Promise<void> {
    // Optimistic dismiss — re-add on failure so the user knows it
    // didn't take. Without the rollback the card would stay hidden
    // until the next poll and the failure would be invisible.
    pendingDismiss.value.add(id)
    try {
      await dismissYskCard(id)
      cards.value = cards.value.filter((c) => c.id !== id)
    } catch (err) {
      pendingDismiss.value.delete(id)
      lastError.value = err instanceof Error ? err.message : String(err)
      throw err
    } finally {
      pendingDismiss.value.delete(id)
    }
  }

  // Visibility handler: refetch when the tab regains focus. This gives
  // us "cross-tab cards stay in sync" behaviour without standing up
  // a full bus WebSocket subscription. If the user dismisses a card
  // on tab A and switches to tab B, the dashboard refetches within
  // tens of milliseconds rather than waiting up to a full poll cycle.
  function onVisibility(): void {
    if (typeof document !== 'undefined' && document.visibilityState === 'visible') {
      fetchCards()
    }
  }

  function startPolling(): void {
    if (pollTimer !== null) return
    fetchCards()
    pollTimer = setInterval(() => {
      fetchCards()
    }, POLL_INTERVAL_MS)
    if (typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', onVisibility)
    }
  }

  function stopPolling(): void {
    if (pollTimer !== null) {
      clearInterval(pollTimer)
      pollTimer = null
    }
    if (typeof document !== 'undefined') {
      document.removeEventListener('visibilitychange', onVisibility)
    }
  }

  // Stop polling AND drop session-bound state on logout. logout() in
  // stores/auth.ts does not reload the page, so the cards fetched for the
  // previous user stayed visible to the next one until their first fetch
  // landed. YSK cards are per-user hints, not public content.
  function _onAuthLogout(): void {
    stopPolling()
    cards.value = []
    pendingDismiss.value = new Set()
    lastError.value = null
    lastFetchedAt.value = 0
    loading.value = false
  }

  if (typeof window !== 'undefined') {
    window.addEventListener('auth-logout', _onAuthLogout)
  }

  // Bus pushes (ysk:card:*) fire fetchCards() immediately — that
  // covers the cross-tab / out-of-band update case. The polling
  // interval above is the safety net for when the bus is down.
  function onBusEvent(eventName: string): void {
    if (eventName === 'ysk:card:upsert' || eventName === 'ysk:card:delete') {
      fetchCards()
    }
  }

  return {
    cards,
    visibleCards,
    loading,
    lastError,
    lastFetchedAt,
    fetchCards,
    dismiss,
    startPolling,
    stopPolling,
    onBusEvent,
  }
})
