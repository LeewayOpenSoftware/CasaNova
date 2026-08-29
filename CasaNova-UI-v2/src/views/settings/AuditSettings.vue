<script setup lang="ts">
// Audit Log settings — Stage 5.2 landing.
//
// Reads from the audit fixture for now; real backend call will be
// GET /v1/sys/audit with the same filter params applied server-side.
// Filters are client-side against the in-memory list for dev.

import { ref, computed, onMounted, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import InputText from 'primevue/inputtext'
import Select from 'primevue/select'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import dayjs from 'dayjs'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import type { AuditEvent, AuditSeverity } from '@/types/audit'
import { systemApi } from '@/api'
import { safeCall } from '@/api/safeCall'

const { t } = useI18n()

// Live events from the backend.
const events = ref<AuditEvent[]>([])
const fixtureMode = ref(false)

onMounted(async () => {
  const result = await safeCall(
    () => systemApi.getAuditEvents({ limit: 500 }),
  )
  fixtureMode.value = result.fixture
  events.value = result.data.events as unknown as AuditEvent[]
})

const severityVisuals: Record<AuditSeverity, { icon: string; classes: string }> = {
  info: { icon: 'Info', classes: 'text-blue-400' },
  warning: { icon: 'AlertTriangle', classes: 'text-amber-400' },
  error: { icon: 'XOctagon', classes: 'text-red-500' },
  critical: { icon: 'Flame', classes: 'text-red-600' },
}

const severityFilter = ref<AuditSeverity | 'all'>('all')
const categoryFilter = ref<string>('all')
const searchText = ref('')
// debouncedSearch lags the input by 180 ms so the filter computed
// doesn't run the full 5-stage chain on every keystroke. The select
// filters (severity / category / range) don't need debouncing — they
// only change on click.
const debouncedSearch = ref('')
let searchDebounce: ReturnType<typeof setTimeout> | null = null
watch(searchText, (v) => {
  if (searchDebounce) clearTimeout(searchDebounce)
  searchDebounce = setTimeout(() => {
    debouncedSearch.value = v
  }, 180)
})
const rangeFilter = ref<'all' | '24h' | '7d'>('all')

const severityOptions = [
  { code: 'all', label: 'settings.audit.all_severities' },
  { code: 'info', label: 'settings.audit.severity.info' },
  { code: 'warning', label: 'settings.audit.severity.warning' },
  { code: 'error', label: 'settings.audit.severity.error' },
  { code: 'critical', label: 'settings.audit.severity.critical' },
] as const

const categoryOptions = [
  { code: 'all', label: 'settings.audit.all_categories' },
  { code: 'auth', label: 'settings.audit.category.auth' },
  { code: 'app', label: 'settings.audit.category.app' },
  { code: 'storage', label: 'settings.audit.category.storage' },
  { code: 'network', label: 'settings.audit.category.network' },
  { code: 'system', label: 'settings.audit.category.system' },
  { code: 'security', label: 'settings.audit.category.security' },
  { code: 'backup', label: 'settings.audit.category.backup' },
] as const

const rangeOptions = [
  { code: 'all', label: 'settings.audit.range.all' },
  { code: '24h', label: 'settings.audit.range.24h' },
  { code: '7d', label: 'settings.audit.range.7d' },
] as const

const severityOptionsLocalized = computed(() =>
  severityOptions.map((o) => ({ code: o.code, label: t(o.label) })),
)
const categoryOptionsLocalized = computed(() =>
  categoryOptions.map((o) => ({ code: o.code, label: t(o.label) })),
)
const rangeOptionsLocalized = computed(() =>
  rangeOptions.map((o) => ({ code: o.code, label: t(o.label) })),
)

const filtered = computed(() => {
  const q = debouncedSearch.value.toLowerCase().trim()
  const now = Date.now()
  const cutoff =
    rangeFilter.value === '24h'
      ? now - 24 * 3600 * 1000
      : rangeFilter.value === '7d'
        ? now - 7 * 24 * 3600 * 1000
        : 0
  return events.value
    .filter((e) => severityFilter.value === 'all' || e.severity === severityFilter.value)
    .filter((e) => categoryFilter.value === 'all' || e.category === categoryFilter.value)
    .filter((e) => {
      if (!q) return true
      return (
        e.actor.toLowerCase().includes(q) ||
        e.message.toLowerCase().includes(q) ||
        e.ip.includes(q)
      )
    })
    .filter((e) => (cutoff ? new Date(e.timestamp).getTime() >= cutoff : true))
    .sort((a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime())
})

const detailVisible = ref(false)
const detailEvent = ref<AuditEvent | null>(null)

function openDetail(ev: AuditEvent) {
  detailEvent.value = ev
  detailVisible.value = true
}

function formatDate(iso: string): string {
  return dayjs(iso).format('MMM D, YYYY HH:mm:ss')
}
</script>

<template>
  <SettingsPage
    icon="FileClock"
    :title="t('settings.audit.title')"
    :description="t('settings.audit.description')"
  >
    <SettingsCard :title="t('settings.audit.filters')">
      <div class="flex flex-wrap items-end gap-3">
        <div class="flex-1 min-w-[180px]">
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.audit.search') }}
          </label>
          <span class="relative block">
            <AppIcon
              name="Search"
              :size="14"
              class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-[var(--cn-text-muted)]"
            />
            <InputText
              v-model="searchText"
              :placeholder="t('settings.audit.search_placeholder')"
              class="w-full pl-9"
            />
          </span>
        </div>
        <div class="min-w-[160px]">
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.audit.severity_label') }}
          </label>
          <Select
            v-model="severityFilter"
            :options="severityOptionsLocalized"
            option-label="label"
            option-value="code"
            class="w-full"
          />
        </div>
        <div class="min-w-[160px]">
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.audit.category_label') }}
          </label>
          <Select
            v-model="categoryFilter"
            :options="categoryOptionsLocalized"
            option-label="label"
            option-value="code"
            class="w-full"
          />
        </div>
        <div class="min-w-[140px]">
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.audit.range_label') }}
          </label>
          <Select
            v-model="rangeFilter"
            :options="rangeOptionsLocalized"
            option-label="label"
            option-value="code"
            class="w-full"
          />
        </div>
      </div>
      <div class="mt-3 text-xs text-[var(--cn-text-muted)]">
        {{ t('settings.audit.match_count', { n: filtered.length }, filtered.length) }}
      </div>
    </SettingsCard>

    <SettingsCard :title="t('settings.audit.events_title')">
      <div class="overflow-hidden rounded-lg border border-[var(--cn-border)]">
        <table class="w-full text-sm">
          <thead class="bg-[var(--cn-surface-hover)] text-xs uppercase tracking-wide text-[var(--cn-text-muted)]">
            <tr>
              <th class="w-10 px-3 py-2"></th>
              <th class="px-3 py-2 text-left font-medium">{{ t('settings.audit.col_time') }}</th>
              <th class="hidden px-3 py-2 text-left font-medium sm:table-cell">{{ t('settings.audit.col_category') }}</th>
              <th class="hidden px-3 py-2 text-left font-medium md:table-cell">{{ t('settings.audit.col_actor') }}</th>
              <th class="px-3 py-2 text-left font-medium">{{ t('settings.audit.col_message') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-[var(--cn-border)]">
            <tr
              v-for="ev in filtered"
              :key="ev.id"
              class="cursor-pointer hover:bg-[var(--cn-surface-hover)]"
              @click="openDetail(ev)"
            >
              <td class="px-3 py-2.5">
                <component
                  :is="severityVisuals[ev.severity].icon"
                  :size="15"
                  :class="severityVisuals[ev.severity].classes"
                />
              </td>
              <td class="px-3 py-2.5 font-mono text-xs text-[var(--cn-text-muted)]">
                {{ formatDate(ev.timestamp) }}
              </td>
              <td class="hidden px-3 py-2.5 text-xs text-[var(--cn-text-muted)] sm:table-cell">
                {{ t(`settings.audit.category.${ev.category}`) }}
              </td>
              <td class="hidden px-3 py-2.5 text-[var(--cn-text)] md:table-cell">
                {{ ev.actor }}
              </td>
              <td class="px-3 py-2.5 text-[var(--cn-text)]">{{ ev.message }}</td>
            </tr>
            <tr v-if="filtered.length === 0">
              <td colspan="5" class="px-3 py-8 text-center text-sm text-[var(--cn-text-muted)]">
                {{ t('settings.audit.empty') }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </SettingsCard>

    <Dialog
      v-model:visible="detailVisible"
      modal
      :style="{ width: '32rem' }"
      :header="t('settings.audit.event_details')"
      :dismissable-mask="true"
    >
      <div v-if="detailEvent" class="flex flex-col gap-3 text-sm">
        <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2">
          <dt class="text-[var(--cn-text-muted)]">{{ t('settings.audit.col_time') }}</dt>
          <dd class="font-mono text-xs">{{ formatDate(detailEvent.timestamp) }}</dd>
          <dt class="text-[var(--cn-text-muted)]">{{ t('settings.audit.severity_label') }}</dt>
          <dd>
            <span class="inline-flex items-center gap-1.5">
              <component
                :is="severityVisuals[detailEvent.severity].icon"
                :size="13"
                :class="severityVisuals[detailEvent.severity].classes"
              />
              {{ t(`settings.audit.severity.${detailEvent.severity}`) }}
            </span>
          </dd>
          <dt class="text-[var(--cn-text-muted)]">{{ t('settings.audit.col_category') }}</dt>
          <dd>{{ t(`settings.audit.category.${detailEvent.category}`) }}</dd>
          <dt class="text-[var(--cn-text-muted)]">{{ t('settings.audit.col_actor') }}</dt>
          <dd>{{ detailEvent.actor }}</dd>
          <dt class="text-[var(--cn-text-muted)]">{{ t('settings.audit.ip') }}</dt>
          <dd class="font-mono">{{ detailEvent.ip }}</dd>
        </dl>
        <div>
          <div class="mb-1 text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.audit.col_message') }}
          </div>
          <div class="rounded-lg bg-[var(--cn-bg)] p-3 text-[var(--cn-text)]">
            {{ detailEvent.message }}
          </div>
        </div>
        <div v-if="detailEvent.metadata">
          <div class="mb-1 text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.audit.metadata') }}
          </div>
          <pre class="overflow-auto rounded-lg bg-[var(--cn-bg)] p-3 font-mono text-xs text-[var(--cn-text)]">{{ JSON.stringify(detailEvent.metadata, null, 2) }}</pre>
        </div>
      </div>
      <template #footer>
        <Button
          :label="t('common.close')"
          severity="secondary"
          @click="detailVisible = false"
        />
      </template>
    </Dialog>
  </SettingsPage>
</template>
