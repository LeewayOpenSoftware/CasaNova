// UpdateManager store — frontend half of the self-update engine.
//
// Responsibilities:
//   - Poll /v1/sys/update/status on a long interval (60s) so the
//     "update available" dot in the topbar stays fresh without
//     hammering the backend.
//   - Short-interval poll (2s) while an update is in progress so the
//     progress bar on the About page animates smoothly.
//   - Expose imperative actions: checkNow, startUpdate, cancelUpdate,
//     changeChannel. Each wraps the raw API call in safeCall() so the
//     UI stays usable in offline / fixture mode.
//   - Push a toast on terminal transitions (completed, failed).
//
// The store does NOT restart the frontend. When the backend enters
// the `restarting` stage, systemd restarts the CasaNova services.
// The axios client will see ERR_NETWORK briefly and the auth
// interceptor will keep retrying; once the new backend is up we'll
// get a `completed` status and the UI can reload itself.
//
// Why a separate store from installProgress: installProgress is the
// compose-app install queue (N concurrent app installs). Updates is
// exactly one system-wide state machine. Different shape, different
// lifetime, different poll cadence.

import { defineStore } from 'pinia'
import { ref, computed, onScopeDispose } from 'vue'
import { updatesApi } from '@/api'
import { safeCall } from '@/api/safeCall'
import type {
  UpdateStatus,
  UpdateChannel,
  ChangelogEntry,
} from '@/api/updates'
import { useInstallProgressStore } from './installProgress'

// How long to wait between status polls when nothing is happening.
const IDLE_POLL_MS = 60_000
// And when an update is in flight.
const ACTIVE_POLL_MS = 2_000

export const useUpdatesStore = defineStore('updates', () => {
  const status = ref<UpdateStatus>({
    currentVersion: '',
    latestVersion: null,
    channel: 'stable',
    updateAvailable: false,
    stage: 'idle',
    progressPercent: 0,
    message: '',
    lastCheckedAt: null,
    lastAppliedAt: null,
    sizeBytes: 0,
    requiresReboot: false,
  } as UpdateStatus)
  const changelog = ref<ChangelogEntry[]>([])
  const loading = ref(false)
  const fixtureMode = ref(false)
  const lastError = ref<string | null>(null)

  // Derived: the update is "active" when the backend is doing work.
  // The UI uses this to decide whether to show a progress bar.
  const isActive = computed(() => {
    const s = status.value.stage
    return (
      s === 'checking' ||
      s === 'downloading' ||
      s === 'verifying' ||
      s === 'applying' ||
      s === 'restarting'
    )
  })

  // Cleaner UI hook than watching stage strings from components.
  const canStart = computed(
    () => !isActive.value && status.value.updateAvailable,
  )
  const canCancel = computed(
    () =>
      status.value.stage === 'downloading' ||
      status.value.stage === 'verifying' ||
      status.value.stage === 'checking',
  )

  // ----- Polling lifecycle -----
  let pollHandle: number | null = null
  // How many mounted consumers want the loop running. AppLayout starts it for
  // the whole session while the settings card starts it again for its own
  // lifetime; without a refcount, unmounting the card stopped the app-wide
  // poller and the update badge went stale until a reload.
  let pollRefs = 0
  // Whether the loop is meant to be running. scheduleNext() consults this so
  // a poll that lands after stopPolling() (logout) cannot resurrect the loop.
  let pollingWanted = false
  let lastStage: UpdateStatus['stage'] = status.value.stage
  let refreshInFlight = false
  let simTimers: number[] = []

  async function refresh(): Promise<void> {
    if (refreshInFlight) return
    refreshInFlight = true
    loading.value = true
    const result = await safeCall(() => updatesApi.getStatus())
    loading.value = false
    if (result.error) {
      lastError.value = result.error
      refreshInFlight = false
      // Keep polling. A failed status read is the NORMAL case mid-update —
      // the box is restarting its own services — and returning here left the
      // progress bar frozen at whatever it last showed, for the rest of the
      // session, with no way back short of a page reload.
      scheduleNext()
      return
    }
    lastError.value = null
    fixtureMode.value = false
    const next = result.data
    // Detect terminal transitions so we can toast exactly once.
    if (lastStage !== next.stage) {
      onStageTransition(lastStage, next.stage, next)
      lastStage = next.stage
    }
    status.value = next
    refreshInFlight = false
    // Re-tune the poll cadence based on the new stage.
    scheduleNext()
  }

  function onStageTransition(
    from: UpdateStatus['stage'],
    to: UpdateStatus['stage'],
    s: UpdateStatus,
  ) {
    const toasts = useInstallProgressStore()
    if (to === 'completed' && from !== 'idle') {
      toasts.pushToast(
        'success',
        `CasaNova ${s.currentVersion} — update complete.`,
      )
    } else if (to === 'failed') {
      toasts.pushToast('error', s.message || 'Update failed.')
    } else if (to === 'restarting') {
      toasts.pushToast('info', 'Restarting services...')
    }
  }

  function scheduleNext() {
    if (pollHandle !== null) {
      window.clearTimeout(pollHandle)
      pollHandle = null
    }
    if (!pollingWanted) return
    const interval = isActive.value ? ACTIVE_POLL_MS : IDLE_POLL_MS
    pollHandle = window.setTimeout(() => {
      void refresh()
    }, interval)
  }

  function startPolling() {
    pollRefs++
    if (pollHandle !== null) return
    pollingWanted = true
    // Defer the first update check so it doesn't compete with the
    // critical container + system utilization fetches on page reload.
    // The update status is non-urgent — a 5-second delay is invisible
    // to the user but lets the dashboard render ~50-100ms faster.
    pollHandle = window.setTimeout(() => {
      void refresh()
    }, 5000)
  }

  // Imperative actions (apply / rollback) need the loop awake right away but
  // do NOT own a lifecycle ref — they never get an unmount to pair with, so
  // going through startPolling() would leak a ref and pin the loop on.
  function wakeUpPolling() {
    pollingWanted = true
    if (pollHandle !== null) {
      window.clearTimeout(pollHandle)
      pollHandle = null
    }
    pollHandle = window.setTimeout(() => {
      void refresh()
    }, 0)
  }

  function stopPolling() {
    if (pollRefs > 0) pollRefs--
    if (pollRefs > 0) return
    pollingWanted = false
    if (pollHandle !== null) {
      window.clearTimeout(pollHandle)
      pollHandle = null
    }
    // Also clear any running fixture simulation timers
    simTimers.forEach(clearTimeout)
    simTimers = []
  }

  // ----- Imperative actions -----

  /**
   * Force a fresh check against the release channel. Unlike refresh()
   * which just reads the cached engine state, this hits the backend's
   * check endpoint which actually queries the remote manifest.
   */
  async function checkNow(): Promise<void> {
    loading.value = true
    lastError.value = null
    const result = await safeCall(
      () => updatesApi.checkForUpdate(),
    )
    loading.value = false
    if (result.error) {
      lastError.value = result.error
      return
    }
    fixtureMode.value = false
    status.value = result.data
    lastStage = result.data.stage
    scheduleNext()
  }

  /**
   * Begin applying the update. Optimistically flip to `downloading`
   * so the UI reacts immediately, then the poll loop takes over and
   * tracks real backend progress.
   */

  // A failed START request does NOT mean the operation did not start. The
  // request can time out client-side while the server is still handling it, and
  // reporting "could not start" then is the most dangerous thing this screen can
  // say: it invites a second click on an operation that must never run twice.
  // Ask the server what actually happened before blaming the user's click.
  async function startReallyFailed(err: string): Promise<boolean> {
    const probe = await safeCall(() => updatesApi.getStatus())
    if (!probe.error && probe.data && probe.data.stage !== 'idle' && probe.data.stage !== 'failed') {
      // It is running. Adopt the real state and say nothing about an error.
      status.value = probe.data
      lastStage = probe.data.stage
      scheduleNext()
      return false
    }
    lastError.value = err
    return true
  }

  async function startUpdate(allowReboot = false): Promise<boolean> {
    if (fixtureMode.value) {
      // Drive a local simulation so the UI is demo-able without a
      // real backend. Real backend: jobId comes back from POST and
      // we start short-poll. Fixture mode: we manually tick.
      simulateLocalUpdate()
      return true
    }
    const result = await safeCall(
      () => updatesApi.startUpdate({ allowReboot }),
    )
    if (result.error) {
      // Confirm with the server before declaring failure — a timed-out ack over
      // a package that is being validated is not a failed start.
      if (await startReallyFailed(result.error)) {
        return false
      }
      return true
    }
    // Flip to active so the short poll kicks in right away.
    status.value = {
      ...status.value,
      stage: 'downloading',
      progressPercent: 0,
      message: 'Downloading update...',
    }
    scheduleNext()
    return true
  }

  async function cancelUpdate(): Promise<boolean> {
    // Clear any fixture simulation timers.
    simTimers.forEach(clearTimeout)
    simTimers = []
    if (fixtureMode.value) {
      status.value = { ...status.value, stage: 'idle', progressPercent: 0, message: 'Update cancelled.' }
      lastStage = 'idle'
      return true
    }
    const result = await safeCall(
      () => updatesApi.cancelUpdate(),
    )
    if (result.error) {
      lastError.value = result.error
      return false
    }
    await refresh()
    return true
  }

  async function clearCache(): Promise<boolean> {
    loading.value = true
    const result = await safeCall(() => updatesApi.clearCache())
    loading.value = false
    if (result.error) { lastError.value = result.error; return false }
    status.value = result.data
    return true
  }

  async function repairSystem(): Promise<boolean> {
    lastError.value = null
    const result = await safeCall(() => updatesApi.repairSystem())
    if (result.error) {
      if (await startReallyFailed(result.error)) {
        return false
      }
      return true
    }
    // The apply engine restarts services; status polling picks up progress.
    await refresh()
    return true
  }

  async function changeChannel(channel: UpdateChannel): Promise<boolean> {
    // Optimistic so the segmented control feels instant; roll back on fail.
    const prevChannel = status.value.channel
    status.value = { ...status.value, channel }
    loading.value = true
    const result = await safeCall(
      () => updatesApi.setChannel(channel),
    )
    loading.value = false
    if (result.error) {
      lastError.value = result.error
      status.value = { ...status.value, channel: prevChannel }
      return false
    }
    status.value = result.data
    // Switching channels usually surfaces a new target, so pull the
    // changelog for it right away.
    await loadChangelog()
    return true
  }

  async function loadChangelog(): Promise<void> {
    const result = await safeCall(
      () => updatesApi.getChangelog(status.value.latestVersion ?? undefined),
    )
    if (result.error) return
    changelog.value = result.data
  }

  // ----- Fixture-mode simulation -----
  // Walks the state machine locally so the About page is demo-able
  // before the backend endpoint exists. Uses the real stage strings
  // so swapping in the backend requires zero UI changes.
  function simulateLocalUpdate() {
    simTimers.forEach(clearTimeout)
    simTimers = []
    const stages: Array<{
      stage: UpdateStatus['stage']
      percent: number
      message: string
      ms: number
    }> = [
      { stage: 'downloading', percent: 15, message: 'Downloading package...', ms: 0 },
      { stage: 'downloading', percent: 55, message: 'Downloading package...', ms: 800 },
      { stage: 'downloading', percent: 95, message: 'Downloading package...', ms: 1600 },
      { stage: 'verifying', percent: 100, message: 'Verifying signature...', ms: 2200 },
      { stage: 'applying', percent: 30, message: 'Writing files...', ms: 2800 },
      { stage: 'applying', percent: 70, message: 'Running migrations...', ms: 3500 },
      { stage: 'restarting', percent: 100, message: 'Restarting services...', ms: 4200 },
      {
        stage: 'completed',
        percent: 100,
        message: 'Update complete.',
        ms: 5200,
      },
    ]
    for (const step of stages) {
      simTimers.push(window.setTimeout(() => {
        const from = status.value.stage
        status.value = {
          ...status.value,
          stage: step.stage,
          progressPercent: step.percent,
          message: step.message,
          currentVersion:
            step.stage === 'completed'
              ? status.value.latestVersion ?? status.value.currentVersion
              : status.value.currentVersion,
          updateAvailable: step.stage !== 'completed',
          lastAppliedAt:
            step.stage === 'completed' ? new Date().toISOString() : status.value.lastAppliedAt,
        }
        onStageTransition(from, step.stage, status.value)
        lastStage = step.stage
      }, step.ms))
    }
    // After the sim, auto-return to idle so the UI is clean.
    simTimers.push(window.setTimeout(() => {
      status.value = {
        ...status.value,
        stage: 'idle',
        progressPercent: 0,
        message: 'CasaNova is up to date.',
      }
      lastStage = 'idle'
    }, 7000))
  }

  // Wave 32: stop polling AND wipe state when the user logs out.
  // Previously this store had no auth-logout hook at all, which meant:
  //   1. logout cleared the bearer token
  //   2. the 60s/2s update poller kept firing with no auth header
  //   3. each tick threw a 401 and the topbar still showed the
  //      previous user's "update available" badge until the page
  //      was hard-reloaded
  // The version strings (currentVersion / latestVersion) and the
  // changelog are arguably innocuous, but the running stage and
  // last-applied timestamp are session-bound state that should not
  // bleed across users on a shared workstation.
  function _onAuthLogout() {
    pollRefs = 0
    stopPolling()
    status.value = {
      currentVersion: '',
      latestVersion: null,
      channel: 'stable',
      updateAvailable: false,
      stage: 'idle',
      progressPercent: 0,
      message: '',
      lastCheckedAt: null,
      lastAppliedAt: null,
      sizeBytes: 0,
      requiresReboot: false,
    } as UpdateStatus
    changelog.value = []
    lastError.value = null
    lastStage = 'idle'
  }
  window.addEventListener('auth-logout', _onAuthLogout)

  // Cleanup all timers when the store scope is disposed (HMR / test).
  onScopeDispose(() => {
    pollRefs = 0
    stopPolling()
    simTimers.forEach(clearTimeout)
    simTimers = []
    window.removeEventListener('auth-logout', _onAuthLogout)
  })

  /**
   * startUpdateFromDisk — triggers an upgrade from a .tar.gz file on
   * the server's local filesystem (USB drive, uploaded file, etc.).
   * Enters the same polling loop as a normal OTA update.
   */
  async function startUpdateFromDisk(filePath: string, allowReboot = false): Promise<boolean> {
    if (fixtureMode.value) {
      // Simulate in fixture mode
      status.value = { ...status.value, stage: 'applying', progressPercent: 10, message: `Applying from ${filePath}…` }
      return true
    }
    const result = await safeCall(
      () => updatesApi.startUpdateFromDisk(filePath, allowReboot),
    )
    if (result.error) {
      lastError.value = result.error
      return false
    }
    // Switch to active polling so the progress bar tracks
    wakeUpPolling()
    return true
  }

  // Roll back to the previous build using the backup the update script
  // wrote before applying. The backend kicks off the rollback script
  // out-of-band — services will restart, so the UI will see a network
  // hiccup before it reads a fresh status.
  async function rollback(): Promise<boolean> {
    if (fixtureMode.value) return true
    const res = await safeCall(() => updatesApi.rollbackUpdate())
    if (res.error) {
      lastError.value = res.error
      return false
    }
    return true
  }

  return {
    status,
    changelog,
    loading,
    fixtureMode,
    lastError,
    isActive,
    canStart,
    canCancel,
    refresh,
    startPolling,
    stopPolling,
    checkNow,
    startUpdate,
    startUpdateFromDisk,
    cancelUpdate,
    changeChannel,
    loadChangelog,
    rollback,
    clearCache,
    repairSystem,
  }
})
