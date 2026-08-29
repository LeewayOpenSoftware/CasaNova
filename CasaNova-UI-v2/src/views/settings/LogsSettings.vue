<script setup lang="ts">
// System Logs settings page — journalctl viewer with filters, search,
// auto-refresh, and color-coded priority levels.

import { ref, computed, onMounted, onUnmounted, nextTick, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import InputText from 'primevue/inputtext'
import Select from 'primevue/select'
import Button from 'primevue/button'
import ToggleSwitch from 'primevue/toggleswitch'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { systemApi } from '@/api'
import { safeCall } from '@/api/safeCall'
import type { LogEntry } from '@/api/system'

const { t } = useI18n()

// ---- State ----

const logs = ref<LogEntry[]>([])
const units = ref<string[]>([])
const loading = ref(false)
const autoRefresh = ref(false)
const autoScroll = ref(true)
const searchText = ref('')

// Filters
const selectedUnit = ref<string>('')
const selectedPriority = ref<string>('all')
const selectedRange = ref<string>('1h')
const selectedLines = ref<number>(200)

let refreshInterval: ReturnType<typeof setInterval> | null = null
const logContainer = ref<HTMLElement | null>(null)

// ---- Options ----

const priorityOptions = computed(() => [
  { code: 'all', label: t('settings.logs.priority_all') },
  { code: 'err', label: t('settings.logs.priority_error') },
  { code: 'warning', label: t('settings.logs.priority_warning') },
  { code: 'info', label: t('settings.logs.priority_info') },
  { code: 'debug', label: t('settings.logs.priority_debug') },
])

const rangeOptions = computed(() => [
  { code: '1h', label: t('settings.logs.range_1h') },
  { code: 'today', label: t('settings.logs.range_today') },
  { code: '24h', label: t('settings.logs.range_24h') },
  { code: '7d', label: t('settings.logs.range_7d') },
  { code: '', label: t('settings.logs.range_all') },
])

const linesOptions = [
  { code: 100, label: '100' },
  { code: 200, label: '200' },
  { code: 500, label: '500' },
  { code: 1000, label: '1000' },
  { code: 2000, label: '2000' },
]

const unitOptions = computed(() => {
  const opts = [{ code: '', label: t('settings.logs.all_units') }]
  for (const u of units.value) {
    opts.push({ code: u, label: u })
  }
  return opts
})

// ---- Filtered logs ----

const filtered = computed(() => {
  const q = searchText.value.toLowerCase().trim()
  if (!q) return logs.value
  return logs.value.filter(
    (e) =>
      e.message.toLowerCase().includes(q) ||
      e.unit.toLowerCase().includes(q) ||
      e.timestamp.includes(q),
  )
})

// ---- Priority styling ----

function priorityClass(p: number): string {
  if (p <= 2) return 'text-red-600 font-semibold' // emerg, alert, crit
  if (p === 3) return 'text-red-500'               // err
  if (p === 4) return 'text-amber-400'             // warning
  if (p === 5) return 'text-cyan-400'              // notice
  if (p === 6) return 'text-[var(--cn-text)]'      // info
  return 'text-[var(--cn-text-muted)]'             // debug
}

function priorityLabel(p: number): string {
  const map: Record<number, string> = {
    0: 'EMERG',
    1: 'ALERT',
    2: 'CRIT',
    3: 'ERR',
    4: 'WARN',
    5: 'NOTICE',
    6: 'INFO',
    7: 'DEBUG',
  }
  return map[p] ?? String(p)
}

function formatTime(iso: string): string {
  try {
    const d = new Date(iso)
    return d.toLocaleTimeString(undefined, {
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    })
  } catch {
    return iso
  }
}

// ---- Data fetching ----

// Every fetch takes a ticket. Only the newest one may write to `logs` —
// journalctl for "all units, last 5000 lines" can take seconds longer than a
// narrow query, so without this the slow answer to the PREVIOUS filter lands
// last and overwrites what the user just asked for.
let logsEpoch = 0

async function fetchLogs() {
  const epoch = ++logsEpoch
  loading.value = true
  const params: Record<string, string | number> = {
    lines: selectedLines.value,
  }
  if (selectedUnit.value) params.unit = selectedUnit.value
  if (selectedPriority.value !== 'all') params.priority = selectedPriority.value
  if (selectedRange.value) params.since = selectedRange.value

  const result = await safeCall(() => systemApi.getSystemLogs(params as any))
  if (epoch !== logsEpoch) return
  if (!result.error) {
    logs.value = result.data ?? []
  }
  loading.value = false

  if (autoScroll.value) {
    await nextTick()
    scrollToBottom()
  }
}

async function fetchUnits() {
  const result = await safeCall(() => systemApi.getLogUnits())
  if (!result.error) {
    units.value = result.data ?? []
  }
}

function scrollToBottom() {
  if (logContainer.value) {
    logContainer.value.scrollTop = logContainer.value.scrollHeight
  }
}

// ---- Auto-refresh ----

watch(autoRefresh, (on) => {
  if (on) {
    refreshInterval = setInterval(fetchLogs, 5000)
  } else if (refreshInterval) {
    clearInterval(refreshInterval)
    refreshInterval = null
  }
})

// Wave 34-Y: previously the filter dropdowns (Unit, Priority, Time
// range, Lines) were bound with v-model but nothing triggered a
// fetch when their values changed. The user would pick a different
// unit or a wider range, nothing happened, and they'd have to hit
// the "Refresh" button to see any change at all. Watch the filter
// tuple and refetch on any change. Bursts are handled by the epoch
// ticket in fetchLogs — stale responses are dropped, not rendered.
watch(
  [selectedUnit, selectedPriority, selectedRange, selectedLines],
  () => {
    fetchLogs()
  },
)

// ---- Lifecycle ----

onMounted(() => {
  fetchUnits()
  fetchLogs()
})

onUnmounted(() => {
  // Wave 34-Y: also forcibly clear the interval and flip the toggle
  // to off on teardown. If the user navigates away while auto-refresh
  // is on, the interval closure holds references to fetchLogs (which
  // hits the authenticated axios client) and keeps firing against
  // the backend after logout — every response would be a 401 that
  // triggers the interceptor's refresh-token dance. Cleanest fix is
  // to null out the interval AND reset the toggle state so a later
  // re-mount starts fresh.
  if (refreshInterval) {
    clearInterval(refreshInterval)
    refreshInterval = null
  }
  autoRefresh.value = false
})
</script>

<template>
  <SettingsPage
    icon="FileText"
    :title="t('settings.logs.title')"
    :description="t('settings.logs.description')"
  >
    <!-- Filter bar -->
    <SettingsCard>
      <div class="flex flex-wrap items-end gap-3">
        <!-- Unit -->
        <div class="min-w-[180px]">
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.logs.unit') }}
          </label>
          <Select
            v-model="selectedUnit"
            :options="unitOptions"
            option-label="label"
            option-value="code"
            class="w-full"
          />
        </div>

        <!-- Priority -->
        <div class="min-w-[140px]">
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.logs.priority') }}
          </label>
          <Select
            v-model="selectedPriority"
            :options="priorityOptions"
            option-label="label"
            option-value="code"
            class="w-full"
          />
        </div>

        <!-- Time range -->
        <div class="min-w-[140px]">
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.logs.time_range') }}
          </label>
          <Select
            v-model="selectedRange"
            :options="rangeOptions"
            option-label="label"
            option-value="code"
            class="w-full"
          />
        </div>

        <!-- Lines -->
        <div class="min-w-[100px]">
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.logs.lines') }}
          </label>
          <Select
            v-model="selectedLines"
            :options="linesOptions"
            option-label="label"
            option-value="code"
            class="w-full"
          />
        </div>

        <!-- Search -->
        <div class="flex-1 min-w-[180px]">
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.logs.search') }}
          </label>
          <span class="relative block">
            <AppIcon
              name="Search"
              :size="14"
              class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-[var(--cn-text-muted)]"
            />
            <InputText
              v-model="searchText"
              :placeholder="t('settings.logs.search_placeholder')"
              class="w-full pl-9"
            />
          </span>
        </div>
      </div>

      <!-- Action row -->
      <div class="mt-3 flex flex-wrap items-center gap-4">
        <Button
          :label="t('settings.logs.refresh')"
          severity="secondary"
          :loading="loading"
          @click="fetchLogs"
        >
          <template #icon>
            <AppIcon name="RefreshCw" :size="14" />
          </template>
        </Button>

        <div class="flex items-center gap-2">
          <ToggleSwitch v-model="autoRefresh" />
          <span class="text-xs text-[var(--cn-text-muted)]">
            {{ t('settings.logs.auto_refresh') }}
          </span>
        </div>

        <div class="flex items-center gap-2">
          <ToggleSwitch v-model="autoScroll" />
          <span class="text-xs text-[var(--cn-text-muted)]">
            {{ t('settings.logs.auto_scroll') }}
          </span>
        </div>

        <span class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.logs.showing', { n: filtered.length }) }}
        </span>
      </div>
    </SettingsCard>

    <!-- Log viewer. Deliberately NOT wrapped in a SettingsCard: the viewer is
         the page, and a card around it only added a second border, a second
         padding ring and a redundant "Log viewer" heading, all paid for in
         viewport height that the log itself needs. -->
      <div
        ref="logContainer"
        class="max-h-[calc(100vh-22rem)] min-h-[300px] overflow-auto rounded-lg border border-[var(--cn-border)] bg-[#0b0e14] p-3 font-mono text-xs leading-5"
      >
        <div v-if="filtered.length === 0" class="flex h-full items-center justify-center py-12 text-[var(--cn-text-muted)]">
          <AppIcon name="Filter" :size="16" class="mr-2" />
          {{ t('settings.logs.empty') }}
        </div>
        <div
          v-for="(entry, i) in filtered"
          :key="`${entry.timestamp}-${entry.unit}-${i}`"
          class="flex gap-2 py-0.5 hover:bg-white/5"
        >
          <span class="shrink-0 text-slate-500">{{ formatTime(entry.timestamp) }}</span>
          <span :class="[priorityClass(entry.priority), 'shrink-0 w-[50px]']">
            {{ priorityLabel(entry.priority) }}
          </span>
          <span class="shrink-0 text-cyan-600 max-w-[200px] truncate">{{ entry.unit }}</span>
          <span class="text-slate-200 break-all">{{ entry.message }}</span>
        </div>
      </div>
  </SettingsPage>
</template>
