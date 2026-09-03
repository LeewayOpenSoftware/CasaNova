// Virtual Machines store — drives VmsView off the live CasaNova-Virt backend.
//
// The backend is deployed and functional (libvirt/QEMU on the host); this
// store is the missing wiring between it and the UI. `available` distinguishes
// "the host can't virtualize / service is down" (show the capability fallback)
// from "here are your VMs".
import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import * as virtApi from '@/api/virt'
import type { VirtInfo, DomainSummary, StoragePool, DomainAction } from '@/api/virt'

export const useVirtStore = defineStore('virt', () => {
  const info = ref<VirtInfo | null>(null)
  const domains = ref<DomainSummary[]>([])
  const pools = ref<StoragePool[]>([])
  const loading = ref(false)
  const available = ref<boolean | null>(null) // null = not checked yet
  const error = ref<string | null>(null)
  // per-domain in-flight action guard so one VM's button doesn't disable all.
  const busy = ref<Set<string>>(new Set())

  const running = computed(() => domains.value.filter((d) => d.status === 'running').length)

  function isBusy(name: string) {
    return busy.value.has(name)
  }

  async function fetchAll() {
    loading.value = true
    error.value = null
    try {
      // info first — it's the availability probe. If it throws, the host
      // has no working hypervisor and we show the fallback instead of a
      // broken list.
      info.value = await virtApi.getVirtInfo()
      available.value = true
      const [d, p] = await Promise.all([virtApi.listDomains(), virtApi.listPools()])
      domains.value = d
      pools.value = p
    } catch (e) {
      available.value = false
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  // Lighter refresh used by polling + after an action — no availability reset.
  async function refresh() {
    if (available.value === false) return
    try {
      const [d, p] = await Promise.all([virtApi.listDomains(), virtApi.listPools()])
      domains.value = d
      pools.value = p
      // keep the header counts fresh
      virtApi.getVirtInfo().then((i) => (info.value = i)).catch(() => {})
    } catch {
      /* transient — next tick retries */
    }
  }

  async function action(name: string, act: DomainAction): Promise<boolean> {
    if (busy.value.has(name)) return false
    busy.value = new Set(busy.value).add(name)
    try {
      await virtApi.domainAction(name, act)
      // libvirt state transitions aren't instant; give it a beat then refresh.
      await new Promise((r) => setTimeout(r, 600))
      await refresh()
      return true
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
      return false
    } finally {
      const next = new Set(busy.value)
      next.delete(name)
      busy.value = next
    }
  }

  async function toggleAutostart(name: string, enabled: boolean): Promise<boolean> {
    try {
      await virtApi.setAutostart(name, enabled)
      await refresh()
      return true
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
      return false
    }
  }

  async function remove(name: string): Promise<boolean> {
    if (busy.value.has(name)) return false
    busy.value = new Set(busy.value).add(name)
    try {
      await virtApi.deleteDomain(name)
      domains.value = domains.value.filter((d) => d.name !== name)
      await refresh()
      return true
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
      return false
    } finally {
      const next = new Set(busy.value)
      next.delete(name)
      busy.value = next
    }
  }

  // Polling — slow (10s) since VM state changes are user-driven, not chatty.
  // ponytail: fixed 10s interval; make adaptive only if it ever feels laggy.
  //
  // Refcounted like system.ts / updates.ts: this store has two independent
  // consumers (the VMs route and the dashboard VMs widget). With a plain
  // boolean timer the first of them to unmount stopped polling for the one
  // still on screen, which shows stale VM state with no error anywhere.
  let timer: ReturnType<typeof setInterval> | null = null
  let pollRefs = 0

  // Skip the tick while the tab is hidden — nobody can see the result and on
  // a NAS every poll is real work on the box. Catch up on the way back.
  function tick() {
    if (typeof document !== 'undefined' && document.hidden) return
    void refresh()
  }

  function onVisibilityChange() {
    if (!document.hidden && timer !== null) void refresh()
  }

  function startPolling() {
    pollRefs++
    if (timer) return
    timer = setInterval(tick, 10000)
    if (typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', onVisibilityChange)
    }
  }
  function stopPolling() {
    if (pollRefs > 0) pollRefs--
    if (pollRefs > 0) return
    if (timer) {
      clearInterval(timer)
      timer = null
      if (typeof document !== 'undefined') {
        document.removeEventListener('visibilitychange', onVisibilityChange)
      }
    }
  }

  // Stop polling AND drop every session-bound ref when the user logs out.
  // logout() in stores/auth.ts does not reload the page, so anything left
  // here stays on screen for whoever logs in next: this store held the
  // previous user's VM names, storage pool names and hypervisor info until
  // the new user's first fetch resolved. Same contract the other ten
  // auth-logout listeners already honour.
  function _onAuthLogout() {
    pollRefs = 0
    stopPolling()
    info.value = null
    domains.value = []
    pools.value = []
    loading.value = false
    available.value = null
    error.value = null
    busy.value = new Set()
  }

  if (typeof window !== 'undefined') {
    window.addEventListener('auth-logout', _onAuthLogout)
  }

  return {
    info, domains, pools, loading, available, error, busy,
    running, isBusy,
    fetchAll, refresh, action, toggleAutostart, remove,
    startPolling, stopPolling,
  }
})
