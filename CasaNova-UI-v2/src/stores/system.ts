// System store — live CPU / memory / uptime feeding the dashboard
// SystemStateWidget and the Storage widget's capacity displays.
//
// Talks to /v1/sys/utilization via the system API module through
// safeCall. Polls every 3 seconds when the dashboard is mounted —
// idempotent start/stop lets route changes pause the poll so we're
// not hammering the endpoint from an invisible tab.
//
// Swap points for production:
//   - POLL_INTERVAL_MS: adjust to taste. Real MessageBus push would
//     replace this entirely.
//   - hardware metadata (CPU model, hostname, etc) is a separate
//     call wired to /v1/sys/hardware and cached for the session.

import { defineStore } from 'pinia'
import { ref, computed, onScopeDispose } from 'vue'
import { systemApi } from '@/api'
import { safeCall } from '@/api/safeCall'

const POLL_INTERVAL_MS = 3_000

// Maximum number of samples kept in the rolling history buffer.
// At a 3-second poll interval this gives ~3 minutes of data.
const HISTORY_SIZE = 60

export interface HistoryEntry {
  timestamp: number
  cpu: number
  memPercent: number
  netUp: number
  netDown: number
}

export const useSystemStore = defineStore('system', () => {
  // Live-updated utilization. Shape mirrors SystemUtilization from
  // the API module but normalized to the values the widgets expect.
  const cpuPercent = ref(0)
  const cpuTempC = ref(0)
  const ramPercent = ref(0)
  const ramUsedGB = ref(0)
  const ramTotalGB = ref(0)
  const uptimeSeconds = ref(0)
  const loadAverage = ref<[number, number, number]>([0, 0, 0])

  // System disk (primary storage)
  const diskUsedBytes = ref(0)
  const diskTotalBytes = ref(0)
  const diskAvailBytes = ref(0)
  const diskHealthy = ref(true)

  // Network speeds (bytes/s). Computed from cumulative byte counters
  // returned by the utilization endpoint, using the delta between polls.
  const netUpBytesPerSec = ref(0)
  const netDownBytesPerSec = ref(0)
  let _prevNetSent = 0
  let _prevNetRecv = 0
  let _prevNetTime = 0

  // Rolling history buffer — the last HISTORY_SIZE poll snapshots.
  // Sparkline charts read from this array reactively.
  const history = ref<HistoryEntry[]>([])

  const fixtureMode = ref(false)
  const lastError = ref<string | null>(null)

  let pollTimer: number | null = null
  let refreshInFlight = false

  const ramUsedBytes = computed(() => ramUsedGB.value * 1024 ** 3)
  const ramTotalBytes = computed(() => ramTotalGB.value * 1024 ** 3)

  async function refresh() {
    if (refreshInFlight) return
    refreshInFlight = true
    try {
      const result = await safeCall(
        () => systemApi.getUtilization(),
      )

      if (result.error || !result.data) {
        lastError.value = result.error ?? 'No data returned'
        return
      }
      lastError.value = null
      fixtureMode.value = false

      cpuPercent.value = Math.round(result.data?.cpu?.percent ?? 0)
      cpuTempC.value = Math.round(result.data?.cpu?.temp ?? 0)
      ramPercent.value = Math.round(result.data?.mem?.usedPercent ?? 0)
      ramUsedGB.value = (result.data?.mem?.used ?? 0) / 1024 ** 3
      ramTotalGB.value = (result.data?.mem?.total ?? 0) / 1024 ** 3
      uptimeSeconds.value = result.data?.uptime ?? 0
      loadAverage.value = result.data?.loadAverage ?? [0, 0, 0]

      // System disk
      if (result.data?.sysDisk) {
        diskUsedBytes.value = result.data.sysDisk.used
        diskTotalBytes.value = result.data.sysDisk.size
        diskAvailBytes.value = result.data.sysDisk.avail
        diskHealthy.value = result.data.sysDisk.health
      }

      // Network speed — the backend returns cumulative byte counters per
      // interface. Sum all active interfaces and compute the delta vs the
      // previous poll to get bytes/sec.
      if (result.data?.net && result.data.net.length > 0) {
        let totalSent = 0
        let totalRecv = 0
        for (const iface of result.data.net) {
          totalSent += iface.bytesSent
          totalRecv += iface.bytesRecv
        }
        const now = Date.now() / 1000
        if (_prevNetTime > 0) {
          const dt = now - _prevNetTime
          if (dt > 0) {
            netUpBytesPerSec.value = Math.max(0, Math.round((totalSent - _prevNetSent) / dt))
            netDownBytesPerSec.value = Math.max(0, Math.round((totalRecv - _prevNetRecv) / dt))
          }
        }
        _prevNetSent = totalSent
        _prevNetRecv = totalRecv
        _prevNetTime = now
      }

      // Push a snapshot into the rolling history buffer.
      const entry: HistoryEntry = {
        timestamp: Date.now(),
        cpu: cpuPercent.value,
        memPercent: ramPercent.value,
        netUp: netUpBytesPerSec.value,
        netDown: netDownBytesPerSec.value,
      }
      history.value.push(entry)
      if (history.value.length > HISTORY_SIZE) {
        history.value.shift()
      }
    } finally {
      refreshInFlight = false
    }
  }

  // The dashboard mounts several consumers of this poll (system widget,
  // network widget, HomeView, ServerSettings). They each start it and each
  // stop it on unmount, so without a refcount the first widget to unmount
  // froze the live numbers for everything still on screen.
  let pollRefs = 0

  function tick() {
    // A backgrounded tab cannot show the result, and on a NAS every tick is
    // real work on the box (utilization + disk + net). Skip while hidden and
    // catch up the moment the tab comes back.
    if (typeof document !== 'undefined' && document.hidden) return
    void refresh()
  }

  function onVisibilityChange() {
    if (!document.hidden && pollTimer !== null) void refresh()
  }

  function startPolling() {
    pollRefs++
    if (pollTimer !== null) return
    void refresh()
    pollTimer = window.setInterval(tick, POLL_INTERVAL_MS)
    document.addEventListener('visibilitychange', onVisibilityChange)
  }

  function stopPolling() {
    if (pollRefs > 0) pollRefs--
    if (pollRefs > 0) return
    if (pollTimer !== null) {
      window.clearInterval(pollTimer)
      pollTimer = null
      document.removeEventListener('visibilitychange', onVisibilityChange)
    }
  }

  // Stop polling AND clear all session-bound state when the user logs
  // out. Wave 32: previously this only stopped the timer, but every
  // ref above kept its last-seen value — so user A's CPU/RAM/disk
  // numbers and the entire history sparkline were still visible to
  // user B during the brief window before B's first poll lands. The
  // private storage figures are arguably the worst leak: they expose
  // disk capacity of a server the user just logged into for the very
  // first time. Reset everything to its constructor default.
  function _onAuthLogout() {
    pollRefs = 0
    stopPolling()
    cpuPercent.value = 0
    cpuTempC.value = 0
    ramPercent.value = 0
    ramUsedGB.value = 0
    ramTotalGB.value = 0
    uptimeSeconds.value = 0
    loadAverage.value = [0, 0, 0]
    diskUsedBytes.value = 0
    diskTotalBytes.value = 0
    diskAvailBytes.value = 0
    diskHealthy.value = true
    netUpBytesPerSec.value = 0
    netDownBytesPerSec.value = 0
    history.value = []
    lastError.value = null
    // Reset the network delta baseline so the first poll after a new
    // login doesn't compute a bogus rate against the previous user's
    // counters (which would yield a huge spike or even a negative
    // number across reboots).
    _prevNetSent = 0
    _prevNetRecv = 0
    _prevNetTime = 0
  }
  window.addEventListener('auth-logout', _onAuthLogout)

  onScopeDispose(() => {
    pollRefs = 0
    stopPolling()
    window.removeEventListener('auth-logout', _onAuthLogout)
  })

  return {
    cpuPercent,
    cpuTempC,
    ramPercent,
    ramUsedGB,
    ramTotalGB,
    uptimeSeconds,
    loadAverage,
    ramUsedBytes,
    ramTotalBytes,
    diskUsedBytes,
    diskTotalBytes,
    diskAvailBytes,
    diskHealthy,
    netUpBytesPerSec,
    netDownBytesPerSec,
    history,
    fixtureMode,
    lastError,
    refresh,
    startPolling,
    stopPolling,
  }
})
