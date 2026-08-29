<script setup lang="ts">
// System state widget — two dials for CPU and RAM, matching the
// CasaOS dashboard's "Estado del sistema" widget.
//
// The dial style is user-configurable via a small picker in the
// widget header. Four variants ship: Ring (default CasaOS look),
// Digital (LED readout), Analog (needle speedometer), and Cockpit
// (Peugeot i-Cockpit neon arcs). Selection persists via useUserPrefs.
//
// Stage 3 uses the local fixture from src/fixtures/dashboard.ts.
// Stage 5 integration swaps the fixture for a GET /v1/sys/utilization
// call with a ~3s poll (or a WebSocket subscription via MessageBus
// once that path is wired up). The component's prop interface is
// already shaped for the real data, so swapping is a one-liner.

import { ref, onMounted, onBeforeUnmount, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import MorphIcon from '@/components/ui/MorphIcon.vue'
import Menu from 'primevue/menu'
import type { MenuItem } from 'primevue/menuitem'
import DialGauge from './DialGauge.vue'
import { useThemeStore, type GaugeStyle } from '@/stores/theme'
import { useSystemStore } from '@/stores/system'
import { useContainersStore } from '@/stores/containers'

const { t } = useI18n()
// Pinia store (not a plain composable) so the gauge style ref is
// guaranteed to survive HMR as a single instance — plain composable
// refs can duplicate across Vite hot reloads and desync from
// components mounted before the reload.
const theme = useThemeStore()
const currentVariant = computed<GaugeStyle>(() => theme.prefs.gaugeStyle)

// Live system metrics sourced from the system store. The store wraps
// GET /v1/sys/utilization via safeCall so the dashboard stays alive
// even when the backend is unreachable — the fallback returns a
// gently-drifting fixture so the gauges still animate. Polling
// starts/stops with the component lifecycle so invisible tabs don't
// hammer the endpoint.
const system = useSystemStore()
const stats = computed(() => ({
  cpuPercent: system.cpuPercent,
  cpuTempC: system.cpuTempC,
  ramPercent: system.ramPercent,
  ramTotalGB: system.ramTotalGB,
  ramUsedGB: system.ramUsedGB,
  uptimeSeconds: system.uptimeSeconds,
}))

onMounted(() => {
  system.startPolling()
})

onBeforeUnmount(() => {
  system.stopPolling()
})

// ----- Gauge style picker -----
// Menu entries map to GaugeStyle values. Each option has a mini
// visual hint (Lucide icon) so the menu is scannable without reading
// the labels. The selected option updates `gaugeStyle` directly —
// useUserPrefs persists it to localStorage via a watcher.
const styleMenu = ref<InstanceType<typeof Menu> | null>(null)
function openStyleMenu(event: MouseEvent) {
  styleMenu.value?.toggle(event)
}

interface StyleOption {
  value: GaugeStyle
  labelKey: string
  icon: string
}
const styleOptions: StyleOption[] = [
  { value: 'ring', labelKey: 'dashboard.gauge.ring', icon: 'Circle' },
  { value: 'digital', labelKey: 'dashboard.gauge.digital', icon: 'MonitorSmartphone' },
  { value: 'analog', labelKey: 'dashboard.gauge.analog', icon: 'Gauge' },
  { value: 'cockpit', labelKey: 'dashboard.gauge.cockpit', icon: 'Zap' },
]

const styleMenuItems = computed<MenuItem[]>(() =>
  styleOptions.map((o) => ({
    label: t(o.labelKey),
    icon: o.value, // store the style key in `icon` — we read it in the slot
    command: () => theme.setGaugeStyle(o.value),
  })),
)

function iconFor(styleKey: string): string {
  return styleOptions.find((o) => o.value === styleKey)?.icon ?? 'Circle'
}

// ----- Expandable per-container breakdown -----
// Click the chevron to expand the widget and reveal a CPU / RAM tab
// strip with a sorted list of containers by the active metric. Lets
// users drill from "my server is at 14%" to "it's 14% because jellyfin
// is transcoding" without leaving the dashboard.
const expanded = ref(false)
const breakdownTab = ref<'cpu' | 'ram'>('cpu')

// Flatten the app→services tree into a flat list for the breakdown.
// Uses the real containers store (which fetches from the backend).
const cStore = useContainersStore()

const runningContainers = computed(() => {
  const flat: Array<{
    id: string; name: string; iconUrl: string
    state: string; cpuPercent: number; memoryMB: number
  }> = []
  for (const app of cStore.apps) {
    for (const svc of app.services) {
      if (svc.state === 'running' || svc.state === 'unhealthy') {
        const label = app.services.length > 1
          ? `${app.title || app.id} · ${svc.id}`
          : (app.title || app.id)
        flat.push({
          id: svc.id,
          name: label,
          iconUrl: app.iconUrl || '',
          state: svc.state,
          cpuPercent: svc.cpuPercent,
          memoryMB: svc.memoryMB,
        })
      }
    }
  }
  return flat
})

// Sorted list for the active tab.
const sortedBreakdown = computed(() => {
  const list = [...runningContainers.value]
  if (breakdownTab.value === 'cpu') {
    list.sort((a, b) => b.cpuPercent - a.cpuPercent)
  } else {
    list.sort((a, b) => b.memoryMB - a.memoryMB)
  }
  return list.slice(0, 8)
})

function formatMem(mb: number): string {
  if (mb >= 1024) return (mb / 1024).toFixed(2) + ' GB'
  return mb.toFixed(0) + ' MB'
}

function toggleExpanded() {
  expanded.value = !expanded.value
}
</script>

<template>
  <div class="cn-widget p-4 md:p-5">
    <div class="mb-4 flex items-center justify-between">
      <h3 class="text-sm font-semibold text-[var(--cn-text)]">
        {{ t('dashboard.system_state') }}
      </h3>
      <div class="flex items-center gap-1">
        <!-- Gauge style picker — small button, same visual weight as
             the expand chevron so the header stays calm. -->
        <button
          type="button"
          class="cn-icon-btn h-7 w-7"
          :aria-label="t('dashboard.gauge.style')"
          :title="t('dashboard.gauge.style')"
          @click="openStyleMenu"
        >
          <AppIcon name="Activity" :size="14" />
        </button>
        <button
          class="cn-icon-btn h-7 w-7"
          :aria-label="t('dashboard.gauge.expand')"
          :title="t('dashboard.gauge.expand')"
          @click="toggleExpanded"
        >
          <MorphIcon :name="expanded ? 'ChevronDown' : 'ChevronRight'" :size="16" />
        </button>
        <Menu ref="styleMenu" :model="styleMenuItems" :popup="true">
          <template #item="{ item, props }">
            <a
              v-bind="props.action"
              :class="[
                'flex cursor-pointer items-center gap-2 px-3 py-2 text-sm',
                currentVariant === item.icon
                  ? 'bg-blue-500/10 text-blue-500'
                  : 'text-[var(--cn-text)]',
              ]"
            >
              <AppIcon :name="iconFor(item.icon as string)" :size="14" />
              <span>{{ item.label }}</span>
            </a>
          </template>
        </Menu>
      </div>
    </div>
    <!-- Centered flex row. The gauges cap their width via `max-w` in
         the child component so even when the widget stretches full
         viewport width (mobile single-column mode), the meters stay
         a sensible ~80px tall rather than ballooning into the
         dashboard. -->
    <div class="flex items-center justify-center gap-4 md:gap-6">
      <DialGauge
        :key="`cpu-${currentVariant}`"
        :value="stats.cpuPercent"
        :label="t('dashboard.cpu')"
        :subtext="`${Math.round(stats.cpuTempC)}°C`"
        :variant="currentVariant"
      />
      <DialGauge
        :key="`ram-${currentVariant}`"
        :value="stats.ramPercent"
        :label="t('dashboard.ram')"
        :subtext="`${stats.ramTotalGB.toFixed(2)} GB`"
        :variant="currentVariant"
      />
    </div>

    <!-- Expanded per-container breakdown — CPU / RAM tab strip and a
         sorted list of running containers by the active metric. Smooth
         expand via the native HTML details animation would require
         extra CSS; we use a simple v-if with a transition instead. -->
    <Transition
      enter-from-class="opacity-0 max-h-0"
      enter-to-class="opacity-100 max-h-[420px]"
      leave-from-class="opacity-100 max-h-[420px]"
      leave-to-class="opacity-0 max-h-0"
      enter-active-class="overflow-hidden transition-all duration-300 ease-out"
      leave-active-class="overflow-hidden transition-all duration-200 ease-in"
    >
      <div v-if="expanded" class="mt-4 border-t border-[var(--cn-border)] pt-3">
        <!-- Tab strip -->
        <div class="mb-2 flex gap-4 text-xs font-semibold uppercase tracking-wider">
          <button
            type="button"
            :class="[
              'border-b-2 pb-1 transition-colors',
              breakdownTab === 'cpu'
                ? 'border-blue-500 text-[var(--cn-text)]'
                : 'border-transparent text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]',
            ]"
            @click="breakdownTab = 'cpu'"
          >
            CPU
          </button>
          <button
            type="button"
            :class="[
              'border-b-2 pb-1 transition-colors',
              breakdownTab === 'ram'
                ? 'border-blue-500 text-[var(--cn-text)]'
                : 'border-transparent text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]',
            ]"
            @click="breakdownTab = 'ram'"
          >
            RAM
          </button>
        </div>

        <!-- Sorted per-container list. Each row is name + current
             usage, right-aligned so the numbers line up. -->
        <ul class="flex flex-col gap-1">
          <li
            v-for="c in sortedBreakdown"
            :key="c.id"
            class="flex items-center justify-between gap-2 rounded-md px-1 py-1 text-xs"
          >
            <div class="flex min-w-0 flex-1 items-center gap-2">
              <div
                class="flex h-5 w-5 shrink-0 items-center justify-center overflow-hidden rounded bg-[var(--cn-surface-hover)]"
              >
                <img
                  v-if="c.iconUrl"
                  :src="c.iconUrl"
                  :alt="c.name"
                  class="h-full w-full object-contain"
                />
                <AppIcon v-else name="Container" :size="12" class="text-[var(--cn-text-muted)]" />
              </div>
              <span class="truncate text-[var(--cn-text)]">{{ c.name }}</span>
            </div>
            <span class="shrink-0 font-mono tabular-nums text-[var(--cn-text-muted)]">
              {{ breakdownTab === 'cpu' ? c.cpuPercent + '%' : formatMem(c.memoryMB) }}
            </span>
          </li>
          <li
            v-if="sortedBreakdown.length === 0"
            class="py-3 text-center text-xs text-[var(--cn-text-muted)]"
          >
            {{ t('dashboard.gauge.no_containers') }}
          </li>
        </ul>
      </div>
    </Transition>
  </div>
</template>
