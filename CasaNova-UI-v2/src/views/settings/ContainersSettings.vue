<script setup lang="ts">
// Containers settings — Stage 5.5 landing.
//
// Iterates the apps fixture (one row per compose project) with an
// aggregated state badge, total CPU / memory across all services in
// the app, auto-update toggle, and a lifecycle button strip. Clicking
// the row navigates to the per-app detail view where every service
// can be edited on its own tab.
//
// The "Resource usage by app" card at the top sums running-service
// stats across every app so operators get a single glance-able view
// of where CPU/RAM is going before drilling into any one project.

import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import AppIcon from '@/components/ui/AppIcon.vue'
import ToggleSwitch from 'primevue/toggleswitch'
import Button from 'primevue/button'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import {
  aggregateAppState,
  type AppEntry,
  type ContainerState,
} from '@/types/containers'
import { useContainersStore } from '@/stores/containers'
import { onMounted } from 'vue'

const { t } = useI18n()
const router = useRouter()

// Read from the live containers store (wrapped around containersApi
// with fixture fallback) so this page reflects the real compose
// project state the moment the backend is reachable.
const containersStore = useContainersStore()
onMounted(() => containersStore.refresh())
const apps = computed<AppEntry[]>(() => containersStore.apps)

function openAppDetail(app: AppEntry) {
  router.push(`/app/apps/${app.id}`)
}

// ----- Aggregate helpers -----
// Per-app totals are computed from the service array so a project
// with 4 containers (Immich) reports its combined CPU/mem footprint.
function appCpuTotal(app: AppEntry): number {
  return app.services.reduce((sum, s) => sum + s.cpuPercent, 0)
}
function appMemTotal(app: AppEntry): number {
  return app.services.reduce((sum, s) => sum + s.memoryMB, 0)
}
function appMemLimit(app: AppEntry): number {
  return app.services.reduce((sum, s) => sum + (s.memoryLimitMB ?? 0), 0)
}
function appUptime(app: AppEntry): number {
  // Report the maximum service uptime — reflects "how long has the
  // oldest container been up" which is the most useful single number.
  return Math.max(0, ...app.services.map((s) => s.uptimeSeconds))
}
function appRestartCount(app: AppEntry): number {
  return app.services.reduce((sum, s) => sum + s.restartCount, 0)
}

// Running = at least one service in running/unhealthy state. Matches
// how the dashboard tile's status dot aggregates the project.
const runningApps = computed(() =>
  apps.value.filter((a) => {
    const state = aggregateAppState(a)
    return state === 'running' || state === 'unhealthy'
  }),
)
const totalCpuPercent = computed(() =>
  Math.round(runningApps.value.reduce((sum, a) => sum + appCpuTotal(a), 0) * 100) / 100,
)
const totalMemoryMB = computed(() =>
  Math.round(runningApps.value.reduce((sum, a) => sum + appMemTotal(a), 0) * 100) / 100,
)
function memPercentOfMax(app: AppEntry): number {
  const limit = appMemLimit(app)
  if (!limit) return 0
  return Math.min(100, (appMemTotal(app) / limit) * 100)
}

const stateVisuals: Record<ContainerState, { icon: string; classes: string }> = {
  running: { icon: 'CircleCheck', classes: 'text-emerald-500' },
  stopped: { icon: 'Square', classes: 'text-slate-400' },
  unhealthy: { icon: 'CircleAlert', classes: 'text-amber-500' },
  updating: { icon: 'Loader', classes: 'text-blue-500' },
  crashed: { icon: 'CircleX', classes: 'text-red-500' },
}

function formatUptime(s: number): string {
  if (s === 0) return '—'
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  if (d > 0) return `${d}d ${h}h`
  const m = Math.floor((s % 3600) / 60)
  return `${h}h ${m}m`
}

async function toggleAutoUpdate(app: AppEntry) {
  // Optimistic flip with rollback on failure.
  const target = containersStore.apps.find((a) => a.id === app.id)
  if (!target) return
  const nextVal = !target.autoUpdate
  target.autoUpdate = nextVal
  let containersApi: typeof import('@/api').containersApi
  let safeCall: typeof import('@/api/safeCall').safeCall
  try {
    ;({ containersApi } = await import('@/api'))
    ;({ safeCall } = await import('@/api/safeCall'))
  } catch {
    // A failed chunk load is exactly as much a failure as the API call
    // below — the optimistic flip must roll back the same way either way.
    target.autoUpdate = !nextVal
    return
  }
  const result = await safeCall(
    () => containersApi.updateApp(app.id, { autoUpdate: nextVal }),
    () => undefined,
  )
  // Roll back on hard failure so the toggle stays in sync.
  if (result.error && !result.fixture) {
    target.autoUpdate = !nextVal
  }
}
</script>

<template>
  <SettingsPage
    icon="Container"
    :title="t('settings.containers.title')"
    :description="t('settings.containers.description')"
  >
    <!-- Resource usage by app — aggregated across the running set. -->
    <SettingsCard
      :title="t('settings.containers.resource_usage')"
      :description="t('settings.containers.resource_usage_desc')"
    >
      <div class="mb-4 grid grid-cols-2 gap-4 sm:grid-cols-3">
        <div>
          <div class="flex items-center gap-1 text-xs text-[var(--cn-text-muted)]">
            <AppIcon name="Container" :size="12" /> {{ t('settings.containers.running_apps') }}
          </div>
          <div class="mt-0.5 text-2xl font-semibold text-[var(--cn-text)]">
            {{ runningApps.length }}
          </div>
        </div>
        <div>
          <div class="flex items-center gap-1 text-xs text-[var(--cn-text-muted)]">
            <AppIcon name="Cpu" :size="12" /> {{ t('settings.containers.total_cpu') }}
          </div>
          <div class="mt-0.5 text-2xl font-semibold text-[var(--cn-text)]">
            {{ totalCpuPercent }}%
          </div>
        </div>
        <div>
          <div class="flex items-center gap-1 text-xs text-[var(--cn-text-muted)]">
            <AppIcon name="MemoryStick" :size="12" /> {{ t('settings.containers.total_memory') }}
          </div>
          <div class="mt-0.5 text-2xl font-semibold text-[var(--cn-text)]">
            {{ totalMemoryMB >= 1024 ? (totalMemoryMB / 1024).toFixed(2) + ' GB' : totalMemoryMB.toFixed(1) + ' MB' }}
          </div>
        </div>
      </div>

      <div
        v-if="runningApps.length === 0"
        class="py-4 text-center text-sm text-[var(--cn-text-muted)]"
      >
        {{ t('settings.containers.no_running') }}
      </div>
    </SettingsCard>

    <!-- Per-app row list. One card per compose project, with a tap
         target on the whole row. -->
    <SettingsCard :title="t('settings.containers.running_title')">
      <div class="flex flex-col gap-2">
        <div
          v-for="a in apps"
          :key="a.id"
          class="cursor-pointer rounded-lg border border-[var(--cn-border)] p-4 transition-colors hover:bg-[var(--cn-surface-hover)]"
          @click="openAppDetail(a)"
        >
          <div class="flex items-start justify-between gap-4">
            <div class="flex min-w-0 flex-1 items-start gap-3">
              <div
                class="flex h-11 w-11 shrink-0 items-center justify-center overflow-hidden rounded-lg"
                :style="{ backgroundColor: a.tileColor ?? 'var(--cn-surface-hover)' }"
              >
                <img
                  v-if="a.iconUrl"
                  :src="a.iconUrl"
                  :alt="a.title"
                  class="h-8 w-8 object-contain"
                />
                <AppIcon v-else name="Container" :size="20" class="text-white" />
              </div>
              <div class="min-w-0 flex-1">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="font-medium text-[var(--cn-text)]">{{ a.title }}</span>
                  <span
                    class="inline-flex items-center gap-1.5 text-xs"
                    :class="stateVisuals[aggregateAppState(a)].classes"
                  >
                    <AppIcon :name="stateVisuals[aggregateAppState(a)].icon" :size="12" />
                    {{ t(`settings.containers.state.${aggregateAppState(a)}`) }}
                  </span>
                  <span
                    v-if="a.services.length > 1"
                    class="inline-flex items-center gap-1 rounded-full border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-2 py-0.5 text-[10px] text-[var(--cn-text-muted)]"
                  >
                    <AppIcon name="Layers" :size="10" />
                    {{ a.services.length }} {{ t('apps.service_plural') }}
                  </span>
                  <span
                    v-if="appRestartCount(a) > 0"
                    class="rounded-full border border-amber-500/40 bg-amber-500/10 px-2 py-0.5 text-[10px] text-amber-500"
                  >
                    {{ t('settings.containers.restarts', { n: appRestartCount(a) }) }}
                  </span>
                </div>
                <div class="mt-1 truncate font-mono text-xs text-[var(--cn-text-muted)]">
                  {{ a.services[0]?.dockerImage }}:{{ a.services[0]?.imageTag }}
                  <span v-if="a.services.length > 1">…</span>
                </div>
                <div class="mt-2 flex flex-wrap gap-4 text-xs text-[var(--cn-text-muted)]">
                  <span>
                    {{ t('settings.containers.uptime') }}:
                    <span class="text-[var(--cn-text)]">{{ formatUptime(appUptime(a)) }}</span>
                  </span>
                  <span>CPU: <span class="text-[var(--cn-text)]">{{ appCpuTotal(a) }}%</span></span>
                  <span>
                    RAM:
                    <span class="text-[var(--cn-text)]">
                      {{ appMemTotal(a) }} / {{ appMemLimit(a) || '∞' }} MB
                    </span>
                  </span>
                </div>
                <!-- Memory bar (moved here from the KPI card, which was
                     rendering the whole app list a second time). -->
                <div class="mt-2 h-1 w-full overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
                  <div
                    class="h-full rounded-full transition-all"
                    :class="memPercentOfMax(a) >= 90 ? 'bg-red-500' : memPercentOfMax(a) >= 75 ? 'bg-amber-500' : 'bg-blue-500'"
                    :style="{ width: (memPercentOfMax(a) || 5) + '%' }"
                  />
                </div>
              </div>
            </div>

            <div class="flex shrink-0 items-center gap-2" @click.stop>
              <label class="flex items-center gap-2 text-xs text-[var(--cn-text-muted)]">
                <span>{{ t('settings.containers.auto_update') }}</span>
                <ToggleSwitch
                  :model-value="a.autoUpdate"
                  @update:model-value="toggleAutoUpdate(a)"
                />
              </label>
              <Button
                v-if="aggregateAppState(a) === 'stopped'"
                size="small"
                severity="secondary"
                :aria-label="t('settings.containers.start')"
                @click="containersStore.startApp(a.id)"
              >
                <template #icon>
                  <AppIcon name="Play" :size="13" />
                </template>
              </Button>
              <Button
                v-else
                size="small"
                severity="secondary"
                :aria-label="t('settings.containers.stop')"
                @click="containersStore.stopApp(a.id)"
              >
                <template #icon>
                  <AppIcon name="Square" :size="13" />
                </template>
              </Button>
              <Button
                size="small"
                severity="secondary"
                :aria-label="t('settings.containers.restart')"
                @click="containersStore.restartApp(a.id)"
              >
                <template #icon>
                  <AppIcon name="RotateCw" :size="13" />
                </template>
              </Button>
            </div>
          </div>
        </div>
      </div>
    </SettingsCard>
  </SettingsPage>
</template>
