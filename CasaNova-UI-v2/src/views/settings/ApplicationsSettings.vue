<script setup lang="ts">
// Applications — a management surface for the container apps, distinct from
// the dashboard tiles. Focus: fine control (update / stop / restart) and an
// "app updates" feed à la an app store. Reuses the containers store + the
// existing app-management API, so this stays in sync with the dashboard.
//
// First cut (we'll refine): update check + lifecycle + a three-dots menu that
// mirrors the dashboard tile menu. Folder-move / deeper management lands later.

import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import AppIcon from '@/components/ui/AppIcon.vue'
import Button from 'primevue/button'
import Menu from 'primevue/menu'
import type { MenuItem } from 'primevue/menuitem'
import { useToast } from 'primevue/usetoast'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { useContainersStore } from '@/stores/containers'
import { aggregateAppState, updateEligibility, type AppEntry } from '@/types/containers'
import { checkForUpdates, pullAppUpdate } from '@/api/containers'

const { t } = useI18n()
const toast = useToast()
const router = useRouter()
const store = useContainersStore()

// Per-app transient state: busy (installing an update), checking (querying the
// registry), and the check result incl. the new tag to show installed→new.
const busy = ref<Record<string, boolean>>({})
const checking = ref<Record<string, boolean>>({})
const updateInfo = ref<Record<string, { hasUpdate: boolean; checked: boolean; newTag?: string }>>({})
const checkingAll = ref(false)
const checkDone = ref(0)
const checkTotal = ref(0)

// Only CasaNova-managed apps get the full lifecycle here; unmanaged
// containers live under Settings → Containers.
const apps = computed<AppEntry[]>(() => store.unifiedApps)
const updatesAvailable = computed(() => apps.value.filter((a) => updateInfo.value[a.id]?.hasUpdate).length)

// The installed image tag(s). One service → its tag; several distinct → a count.
function installedTag(app: AppEntry): string {
  const tags = [...new Set(app.services.map((s) => s.imageTag).filter(Boolean))]
  if (tags.length === 0) return '—'
  if (tags.length === 1) return tags[0]
  return t('settings.apps.multi_tags', { n: tags.length })
}

function statusOf(app: AppEntry) { return aggregateAppState(app) }
function statusClass(app: AppEntry) {
  switch (statusOf(app)) {
    case 'running': return 'bg-emerald-500'
    case 'unhealthy': return 'bg-amber-500'
    case 'updating': return 'bg-blue-500'
    case 'crashed': return 'bg-red-500'
    default: return 'bg-gray-500'
  }
}
function isRunning(app: AppEntry) { const s = statusOf(app); return s === 'running' || s === 'unhealthy' }

async function refresh() { await store.refresh(true) }

// Run an async fn over items with a bounded concurrency. Each per-app check
// hits the registry for a digest comparison; firing all ~15 at once on a cold
// start (no auth token / DNS / RepoDigests warmed) saturates and times them
// out, so the backend returns "no update" for many — the "check twice, only
// the second lists them" bug. A small pool lets each check actually complete
// on the first pass.
async function runPool<T>(items: T[], limit: number, fn: (item: T) => Promise<void>) {
  let i = 0
  const worker = async () => {
    while (i < items.length) {
      const idx = i++
      await fn(items[idx])
    }
  }
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker))
}

async function checkOne(a: AppEntry) {
  checking.value[a.id] = true
  try {
    const r = await checkForUpdates(a.id)
    const svc = r.services.find((s) => s.hasUpdate)
    updateInfo.value[a.id] = { hasUpdate: !!svc, checked: true, newTag: svc?.newTag }
  } catch {
    updateInfo.value[a.id] = { hasUpdate: false, checked: true }
  } finally {
    checking.value[a.id] = false
    checkDone.value++
  }
}

async function checkAll() {
  // Only compose apps expose a registry-digest check; others are skipped.
  const managed = apps.value.filter((a) => updateEligibility(a).method === 'compose')
  checkingAll.value = true
  checkDone.value = 0
  checkTotal.value = managed.length
  await runPool(managed, 3, checkOne)
  checkingAll.value = false
  const n = updatesAvailable.value
  toast.add({ severity: n > 0 ? 'info' : 'success', summary: n > 0 ? t('settings.apps.updates_found', { n }) : t('settings.apps.all_current'), life: 3000 })
}

async function doUpdate(app: AppEntry) {
  busy.value[app.id] = true
  try {
    await pullAppUpdate(app.id, !updateInfo.value[app.id]?.hasUpdate)
    toast.add({ severity: 'info', summary: t('settings.apps.updating', { name: app.title }), life: 3000 })
    updateInfo.value[app.id] = { hasUpdate: false, checked: true }
    setTimeout(refresh, 4000)
  } catch (e) {
    toast.add({ severity: 'error', summary: t('settings.apps.update_failed'), detail: String(e), life: 5000 })
  } finally { busy.value[app.id] = false }
}

async function lifecycle(app: AppEntry, action: 'start' | 'stop' | 'restart') {
  busy.value[app.id] = true
  const fn = action === 'start' ? store.startAppUnified : action === 'stop' ? store.stopAppUnified : store.restartAppUnified
  const okr = await fn(app.id)
  busy.value[app.id] = false
  if (!okr) toast.add({ severity: 'error', summary: t('settings.apps.action_failed', { action }), life: 4000 })
}

// Three-dots menu — mirrors the dashboard tile menu, routed to the app detail
// page (which already hosts terminal/logs/full settings).
const menuRef = ref<InstanceType<typeof Menu> | null>(null)
const menuApp = ref<AppEntry | null>(null)
function openMenu(e: MouseEvent, app: AppEntry) { menuApp.value = app; menuRef.value?.toggle(e) }
const menuItems = computed<MenuItem[]>(() => {
  const app = menuApp.value
  if (!app) return []
  const running = isRunning(app)
  return [
    { label: t('dashboard.app_menu.settings'), icon: 'Settings', command: () => router.push(`/app/apps/${app.id}`) },
    running
      ? { label: t('dashboard.app_menu.stop'), icon: 'Square', command: () => lifecycle(app, 'stop') }
      : { label: t('dashboard.app_menu.start'), icon: 'Play', command: () => lifecycle(app, 'start') },
    { label: t('dashboard.app_menu.restart'), icon: 'RotateCw', command: () => lifecycle(app, 'restart') },
    { label: t('dashboard.app_menu.repair'), icon: 'Wrench', command: () => { void onRepair(app) } },
    { separator: true },
    { label: t('apps.uninstall'), icon: 'Trash2', command: () => onUninstall(app) },
  ]
})
async function onRepair(app: AppEntry) {
  busy.value[app.id] = true
  let okr = false
  try {
    okr = await store.repairApp(app.id)
  } catch {
    okr = false
  } finally {
    busy.value[app.id] = false
  }
  toast.add({ severity: okr ? 'success' : 'error', summary: okr ? t('settings.apps.repaired', { name: app.title }) : t('settings.apps.repair_failed'), life: 4000 })
}
async function onUninstall(app: AppEntry) {
  if (!confirm(t('settings.apps.uninstall_confirm', { name: app.title }))) return
  busy.value[app.id] = true
  const okr = await store.uninstallApp(app.id); busy.value[app.id] = false
  toast.add({ severity: okr ? 'success' : 'error', summary: okr ? t('settings.apps.uninstalled', { name: app.title }) : t('settings.apps.action_failed', { action: 'uninstall' }), life: 4000 })
}

onMounted(() => { if (!store.unifiedApps.length) store.refresh() })
</script>

<template>
  <SettingsPage icon="LayoutGrid" :title="t('settings.apps.title')" :description="t('settings.apps.description')">
    <SettingsCard>
      <template #actions>
        <Button size="small" severity="secondary" :loading="checkingAll" @click="checkAll">
          <AppIcon name="RefreshCw" :size="15" class="mr-1.5" />{{ t('settings.apps.check_updates') }}
        </Button>
      </template>
      <!-- Overall check progress — while the registry sweep runs -->
      <div v-if="checkingAll" class="mb-3 flex flex-col gap-1.5">
        <div class="flex items-center gap-2 text-xs text-[var(--cn-text-muted)]">
          <AppIcon name="Loader2" :size="14" class="animate-spin text-blue-500" />
          {{ t('settings.apps.checking_progress', { done: checkDone, total: checkTotal }) }}
        </div>
        <div class="h-1 w-full overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
          <div class="h-full rounded-full bg-blue-500 transition-all" :style="{ width: (checkTotal ? Math.round(checkDone / checkTotal * 100) : 0) + '%' }" />
        </div>
      </div>

      <div v-if="updatesAvailable > 0" class="mb-3 flex items-center gap-2 rounded-lg bg-blue-500/10 px-3 py-2 text-xs text-blue-500">
        <AppIcon name="ArrowUpCircle" :size="15" />{{ t('settings.apps.updates_badge', { n: updatesAvailable }) }}
      </div>

      <div v-if="apps.length" class="flex flex-col gap-2">
        <div v-for="app in apps" :key="app.id" class="flex flex-col gap-2 rounded-lg border border-[var(--cn-border)] p-2.5">
          <div class="flex items-center gap-3">
            <!-- icon -->
            <div class="flex h-10 w-10 shrink-0 items-center justify-center overflow-hidden rounded-lg bg-[var(--cn-surface-hover)]">
              <img v-if="app.iconUrl" :src="app.iconUrl" :alt="app.title" class="h-full w-full object-cover" @error="($event.target as HTMLImageElement).style.display='none'" />
              <span v-else class="text-sm font-semibold text-[var(--cn-text-muted)]">{{ app.title.charAt(0).toUpperCase() }}</span>
            </div>
            <!-- name + status + version -->
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2">
                <span class="truncate text-sm font-medium text-[var(--cn-text)]">{{ app.title }}</span>
                <span class="h-2 w-2 shrink-0 rounded-full" :class="statusClass(app)" />
                <span v-if="checking[app.id]" class="flex items-center gap-1 text-[10px] text-blue-500"><AppIcon name="Loader2" :size="11" class="animate-spin" />{{ t('settings.apps.checking') }}</span>
                <span v-else-if="updateInfo[app.id]?.hasUpdate" class="rounded bg-blue-500/15 px-1.5 py-0.5 text-[10px] font-medium text-blue-500">{{ t('settings.apps.update_tag') }}</span>
              </div>
              <div class="truncate text-xs text-[var(--cn-text-muted)]">
                <span>{{ t('settings.apps.services', { n: app.services.length }, app.services.length) }} · {{ statusOf(app) }}</span>
                <!-- installed → new when an update is available; else just installed -->
                <span v-if="updateInfo[app.id]?.hasUpdate && updateInfo[app.id]?.newTag" class="ml-1 font-mono">
                  · <span class="text-[var(--cn-text-muted)]">{{ installedTag(app) }}</span> <span class="text-blue-500">→ {{ updateInfo[app.id]?.newTag }}</span>
                </span>
                <span v-else class="ml-1 font-mono opacity-70">· {{ installedTag(app) }}</span>
              </div>
            </div>
            <!-- actions -->
            <div class="flex shrink-0 items-center gap-1.5">
              <Button v-if="updateInfo[app.id]?.hasUpdate" size="small" severity="info" :loading="busy[app.id]" @click="doUpdate(app)">
                <AppIcon name="Download" :size="14" class="mr-1" />{{ t('settings.apps.update') }}
              </Button>
              <Button size="small" severity="secondary" text :disabled="busy[app.id]" @click="lifecycle(app, isRunning(app) ? 'stop' : 'start')">
                <AppIcon :name="isRunning(app) ? 'Square' : 'Play'" :size="14" />
              </Button>
              <Button size="small" severity="secondary" text :disabled="busy[app.id]" @click="lifecycle(app, 'restart')">
                <AppIcon name="RotateCw" :size="14" />
              </Button>
              <Button size="small" severity="secondary" text @click="openMenu($event, app)">
                <AppIcon name="MoreVertical" :size="16" />
              </Button>
            </div>
          </div>
          <!-- per-app install progress (indeterminate — the pull runs server-side) -->
          <div v-if="busy[app.id]" class="flex flex-col gap-1">
            <span class="text-[10px] text-blue-500">{{ t('settings.apps.installing') }}</span>
            <div class="h-1 w-full overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
              <div class="cn-indeterminate h-full w-1/3 rounded-full bg-blue-500" />
            </div>
          </div>
        </div>
      </div>
      <p v-else-if="!store.loading" class="text-sm text-[var(--cn-text-muted)]">{{ t('settings.apps.none') }}</p>

      <Menu ref="menuRef" :model="menuItems" :popup="true">
        <template #item="{ item, props }">
          <a class="flex cursor-pointer items-center gap-2 px-3 py-2 text-sm text-[var(--cn-text)]" v-bind="props.action">
            <AppIcon v-if="item.icon" :name="item.icon as string" :size="15" class="text-[var(--cn-text-muted)]" />
            <span>{{ item.label }}</span>
          </a>
        </template>
      </Menu>
    </SettingsCard>
  </SettingsPage>
</template>
