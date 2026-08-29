<script setup lang="ts">
// DiagnosisSettings — surface for the pkg/diagnosis engine (10 parallel
// checks server-side, 5-min cache). The engine existed for months with a
// registered endpoint and an orphaned TS client and NO view — this is the
// missing half. Grouped by category, severity-first ordering inside each
// group, with the backend's own `fix` hint rendered as a copyable command.

import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import dayjs from 'dayjs'
import Button from 'primevue/button'
import AppIcon from '@/components/ui/AppIcon.vue'
import StatusChip from '@/components/ui/StatusChip.vue'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { safeCall } from '@/api/safeCall'
import { systemApi } from '@/api'
import type { DiagnosisResponse, DiagnosticResult, DiagnosisSeverity } from '@/api/system'

const { t } = useI18n()

const data = ref<DiagnosisResponse | null>(null)
const loading = ref(false)
const lastError = ref('')

type ChipTone = 'success' | 'warning' | 'danger' | 'info' | 'neutral'
const severityVisuals: Record<DiagnosisSeverity, { icon: string; tone: ChipTone; classes: string; rank: number }> = {
  error:   { icon: 'XCircle',       tone: 'danger',  classes: 'text-[var(--cn-danger)]',  rank: 0 },
  warning: { icon: 'AlertTriangle', tone: 'warning', classes: 'text-[var(--cn-warning)]', rank: 1 },
  info:    { icon: 'Info',          tone: 'info',    classes: 'text-[var(--cn-accent)]',  rank: 2 },
  ok:      { icon: 'CheckCircle2',  tone: 'success', classes: 'text-[var(--cn-success)]', rank: 3 },
}

const categoryOrder = ['services', 'system', 'storage', 'network', 'container', 'security'] as const

const groups = computed(() => {
  if (!data.value) return []
  const byCat = data.value.by_category
  const known = categoryOrder
    .filter((c) => (byCat[c] ?? []).length > 0)
    .map((c) => ({ category: c, items: sortBySeverity(byCat[c]!) }))
  // Anything the backend adds later (or panic-fallback results with "")
  // still renders instead of silently disappearing.
  const extras = Object.entries(byCat)
    .filter(([c]) => !(categoryOrder as readonly string[]).includes(c))
    .map(([c, items]) => ({ category: c || 'system', items: sortBySeverity(items!) }))
  return [...known, ...extras]
})

function sortBySeverity(items: DiagnosticResult[]): DiagnosticResult[] {
  return [...items].sort((a, b) => severityVisuals[a.severity].rank - severityVisuals[b.severity].rank)
}

const checkedAt = computed(() => {
  const first = data.value?.results?.[0]
  return first ? dayjs(first.checked_at).format('HH:mm:ss') : ''
})

async function load(force: boolean) {
  loading.value = true
  lastError.value = ''
  const res = await safeCall(() => systemApi.getDiagnosis(force))
  loading.value = false
  if (res.error && !res.fixture) {
    lastError.value = res.error
    return
  }
  if (res.data) data.value = res.data
}

onMounted(() => void load(false))
</script>

<template>
  <SettingsPage
    icon="Stethoscope"
    :title="t('settings.diagnosis.title')"
    :description="t('settings.diagnosis.desc')"
  >
    <SettingsCard>
      <template #actions>
        <span v-if="checkedAt" class="mr-2 text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.diagnosis.checked_at', { time: checkedAt }) }}
        </span>
        <Button size="small" :loading="loading" @click="load(true)">
          <AppIcon name="RefreshCw" :size="14" class="mr-1" />
          {{ t('settings.diagnosis.run') }}
        </Button>
      </template>

      <!-- Summary strip -->
      <div v-if="data" class="mb-4 flex flex-wrap items-center gap-2">
        <StatusChip
          v-for="sev in (['error', 'warning', 'info', 'ok'] as const)"
          v-show="(data.summary[sev] ?? 0) > 0"
          :key="sev"
          :tone="severityVisuals[sev].tone"
          :icon="severityVisuals[sev].icon"
        >
          {{ data.summary[sev] }} {{ t('settings.diagnosis.sev_' + sev) }}
        </StatusChip>
      </div>

      <p v-if="lastError" class="mb-3 text-sm text-red-500">{{ lastError }}</p>
      <p v-else-if="!data && loading" class="text-sm text-[var(--cn-text-muted)]">
        {{ t('settings.diagnosis.running') }}
      </p>

      <!-- Grouped results -->
      <div v-for="group in groups" :key="group.category" class="mb-4">
        <h3 class="mb-2 text-xs font-semibold uppercase tracking-wide text-[var(--cn-text-muted)]">
          {{ t('settings.diagnosis.cat_' + group.category) }}
        </h3>
        <div class="flex flex-col gap-1.5">
          <div
            v-for="(item, idx) in group.items"
            :key="group.category + ':' + item.check + ':' + idx"
            class="rounded-lg border border-[var(--cn-border)] p-2.5"
          >
            <div class="flex items-start gap-2.5">
              <AppIcon
                :name="severityVisuals[item.severity].icon"
                :size="16"
                :class="[severityVisuals[item.severity].classes, 'mt-0.5 shrink-0']"
              />
              <div class="min-w-0 flex-1">
                <div class="flex flex-wrap items-baseline gap-x-2">
                  <span class="text-sm font-medium text-[var(--cn-text)]">{{ item.title }}</span>
                  <span class="text-xs text-[var(--cn-text-muted)]">{{ item.summary }}</span>
                </div>
                <p v-if="item.details" class="mt-1 text-xs text-[var(--cn-text-muted)]">
                  {{ item.details }}
                </p>
                <div
                  v-if="item.fix"
                  class="mt-1.5 overflow-x-auto rounded bg-[var(--cn-surface-hover)] px-2 py-1 font-mono text-[11px] text-[var(--cn-text)]"
                >
                  {{ item.fix }}
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </SettingsCard>
  </SettingsPage>
</template>
