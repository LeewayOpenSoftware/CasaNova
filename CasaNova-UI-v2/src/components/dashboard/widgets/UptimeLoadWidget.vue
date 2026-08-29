<script setup lang="ts">
// Uptime & load widget — shows how long the box has been up (prominent)
// plus the 1/5/15-minute load averages as small labelled numbers.
//
// Reads reactive values straight from the system store. The dashboard
// already calls system.startPolling(), so this widget deliberately does
// NOT start its own poll or timer — it just renders whatever the store
// most recently fetched from /v1/sys/utilization.

import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useSystemStore } from '@/stores/system'

const { t } = useI18n()
const system = useSystemStore()

// loadAverage may not be surfaced by every backend (or every version of
// the store), so read it through a narrow optional shape rather than
// assuming the field is present. Falls back to all-zeros, which the
// template still renders — uptime stays useful on its own.
type LoadTuple = [number, number, number]
const loadAverage = computed<LoadTuple>(() => {
  const raw = (system as unknown as { loadAverage?: readonly number[] }).loadAverage
  if (raw && raw.length >= 3) {
    return [raw[0] ?? 0, raw[1] ?? 0, raw[2] ?? 0]
  }
  return [0, 0, 0]
})

// Format seconds as "Nd Nh Nm", dropping leading zero units.
// Examples: 271440 -> "3d 3h 24m", 720 -> "12m", 0 -> "0m".
const uptimeLabel = computed(() => {
  const total = Math.max(0, Math.floor(system.uptimeSeconds))
  const days = Math.floor(total / 86_400)
  const hours = Math.floor((total % 86_400) / 3_600)
  const minutes = Math.floor((total % 3_600) / 60)

  const parts: string[] = []
  if (days > 0) parts.push(`${days}d`)
  if (hours > 0 || days > 0) parts.push(`${hours}h`)
  parts.push(`${minutes}m`)
  return parts.join(' ')
})

const hasUptime = computed(() => system.uptimeSeconds > 0)

// The three labelled load figures, formatted to 2 decimals.
const loads = computed(() => [
  { key: '1m', value: loadAverage.value[0] },
  { key: '5m', value: loadAverage.value[1] },
  { key: '15m', value: loadAverage.value[2] },
])
</script>

<template>
  <div class="cn-widget p-4 md:p-5">
    <div class="mb-3 flex items-center gap-2">
      <AppIcon name="Timer" :size="14" class="text-[var(--cn-text-muted)]" />
      <h3 class="text-sm font-semibold text-[var(--cn-text)]">
        {{ t('dashboard.widgets.uptime.title') }}
      </h3>
    </div>

    <!-- Uptime — the headline figure, shown regardless of load data. -->
    <div class="text-2xl font-light tabular-nums tracking-tight text-[var(--cn-text)] md:text-3xl">
      <template v-if="hasUptime">{{ uptimeLabel }}</template>
      <span v-else class="text-base text-[var(--cn-text-muted)]">
        {{ t('dashboard.widgets.uptime.empty') }}
      </span>
    </div>
    <div class="mt-0.5 text-xs text-[var(--cn-text-muted)]">
      {{ t('dashboard.widgets.uptime.subtitle') }}
    </div>

    <!-- Load averages — three small labelled numbers (1m / 5m / 15m). -->
    <div class="mt-4 border-t border-[var(--cn-border)] pt-3">
      <div class="mb-2 text-[10px] font-semibold uppercase tracking-wider text-[var(--cn-text-muted)]">
        {{ t('dashboard.widgets.uptime.load') }}
      </div>
      <div class="grid grid-cols-3 gap-2">
        <div
          v-for="l in loads"
          :key="l.key"
          class="flex flex-col items-center rounded-md bg-[var(--cn-surface-hover)] py-2"
        >
          <span class="text-sm font-medium tabular-nums text-[var(--cn-text)]">
            {{ l.value.toFixed(2) }}
          </span>
          <span class="mt-0.5 text-[10px] text-[var(--cn-text-muted)]">
            {{ t(`dashboard.widgets.uptime.load_${l.key}`) }}
          </span>
        </div>
      </div>
    </div>
  </div>
</template>
