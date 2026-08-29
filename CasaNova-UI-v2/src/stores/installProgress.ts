// Install progress store — tracks compose-app installs that are
// currently in flight so the dashboard can render a progress card
// while images pull and containers start.
//
// Also doubles as the tiny toast queue so every component can push
// user-visible feedback without passing a toast service through
// props. Pinia gives us a singleton that survives HMR — critical
// after the composable reactivity bug we hit earlier.
//
// Flow:
//   1. User submits the install dialog → beginInstall(name, iconUrl)
//      returns an install id, the dashboard card starts animating
//   2. A fake 5s progress timer ticks the install from 0→100
//   3. On completion, the install is removed from the active list
//      and a toast is pushed
// Real backend swap: replace the fake timer with SSE from
// /v2/app_management/compose/:id/progress.

import { defineStore } from 'pinia'
import { ref, onScopeDispose } from 'vue'

export interface ActiveInstall {
  id: string
  name: string
  iconUrl?: string
  progress: number // 0-100
  startedAt: number
}

export interface ToastMessage {
  id: string
  kind: 'success' | 'error' | 'info'
  text: string
}

export const useInstallProgressStore = defineStore('installProgress', () => {
  const active = ref<ActiveInstall[]>([])
  const toasts = ref<ToastMessage[]>([])

  // Track all timers so they can be cleaned up on cancel/dispose.
  const timers = new Map<string, { interval: number; timeout: number | null }>()

  let seq = 0
  function nextId(prefix: string): string {
    seq++
    return `${prefix}-${Date.now()}-${seq}`
  }

  /**
   * Start a fake install. Creates an ActiveInstall entry, ticks the
   * progress bar over 5 seconds, then removes it and pushes a
   * success toast. Returns the install id so callers can track or
   * cancel.
   */
  function beginInstall(name: string, iconUrl?: string): string {
    const id = nextId('install')
    const entry: ActiveInstall = {
      id,
      name,
      iconUrl,
      progress: 0,
      startedAt: Date.now(),
    }
    active.value = [...active.value, entry]

    // Tick progress every 100ms; reach 100% at 5s.
    const totalMs = 5000
    const tickMs = 100
    const step = 100 / (totalMs / tickMs)
    const handle = window.setInterval(() => {
      const idx = active.value.findIndex((i) => i.id === id)
      if (idx === -1) {
        clearTimers(id)
        return
      }
      const next = [...active.value]
      next[idx] = { ...next[idx], progress: Math.min(100, next[idx].progress + step) }
      active.value = next
      if (next[idx].progress >= 100) {
        window.clearInterval(handle)
        // Small tail so the user sees the bar fill before it vanishes.
        const timeout = window.setTimeout(() => {
          finishInstall(id)
          pushToast('success', `${name} installed successfully`)
        }, 400)
        const t = timers.get(id)
        if (t) t.timeout = timeout
      }
    }, tickMs)
    timers.set(id, { interval: handle, timeout: null })
    return id
  }

  function clearTimers(id: string) {
    const t = timers.get(id)
    if (t) {
      window.clearInterval(t.interval)
      if (t.timeout !== null) window.clearTimeout(t.timeout)
      timers.delete(id)
    }
  }

  function finishInstall(id: string) {
    clearTimers(id)
    active.value = active.value.filter((i) => i.id !== id)
  }

  function cancelInstall(id: string) {
    clearTimers(id)
    active.value = active.value.filter((i) => i.id !== id)
    pushToast('info', 'Install cancelled')
  }

  // ----- Toasts -----
  const toastTimers = new Map<string, number>()

  const MAX_TOASTS = 5

  function pushToast(kind: ToastMessage['kind'], text: string): string {
    const id = nextId('toast')
    toasts.value = [...toasts.value, { id, kind, text }]
    // Evict oldest toasts if we exceed the cap so the stack stays bounded.
    while (toasts.value.length > MAX_TOASTS) {
      const oldest = toasts.value[0]
      dismissToast(oldest.id)
    }
    toastTimers.set(id, window.setTimeout(() => {
      dismissToast(id)
    }, 4000))
    return id
  }

  function dismissToast(id: string) {
    toasts.value = toasts.value.filter((t) => t.id !== id)
    const h = toastTimers.get(id)
    if (h !== undefined) { clearTimeout(h); toastTimers.delete(id) }
  }

  // Wave 33: previously the only cleanup hook was onScopeDispose,
  // which fires on full page unload / HMR / test teardown — NOT on
  // logout. The fake-progress setInterval kept ticking after a
  // logout, mutating active.value for the next user; the 4-second
  // toast setTimeouts kept firing across the auth boundary; and any
  // toast still on screen at the moment of logout (e.g. an error
  // message containing a real filesystem path) stayed rendered for
  // user B. Listen for the logout signal the auth store dispatches
  // and tear down every timer + clear all reactive state.
  function _onAuthLogout() {
    for (const id of [...timers.keys()]) clearTimers(id)
    for (const h of toastTimers.values()) clearTimeout(h)
    toastTimers.clear()
    active.value = []
    toasts.value = []
  }
  window.addEventListener('auth-logout', _onAuthLogout)

  // Cleanup all timers when the store's scope is disposed (HMR, test teardown).
  onScopeDispose(() => {
    for (const id of timers.keys()) clearTimers(id)
    for (const h of toastTimers.values()) clearTimeout(h)
    window.removeEventListener('auth-logout', _onAuthLogout)
  })

  return {
    active,
    toasts,
    beginInstall,
    cancelInstall,
    finishInstall,
    pushToast,
    dismissToast,
  }
})
