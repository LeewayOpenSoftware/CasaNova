<script setup lang="ts">
// Network traffic widget — current up/down rates plus a small history
// graph. Wired to the system store which polls /v1/sys/utilization and
// keeps a rolling history buffer. We only read reactively; the
// SystemStateWidget already owns the poll lifecycle, but we still call
// startPolling here (idempotent) so the widget works standalone too.

import { computed, onMounted, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import SparklineChart from '../SparklineChart.vue'
import { useSystemStore } from '@/stores/system'

const { t } = useI18n()
const system = useSystemStore()

onMounted(() => {
  system.startPolling()
})

onBeforeUnmount(() => {
  system.stopPolling()
})

// Rolling history buffer capacity — mirrors HISTORY_SIZE in the store
// so the sparkline treats the graph as a fixed-width rolling window.
const HISTORY_CAPACITY = 60

// Down / up sample series pulled reactively from the store history.
const downSeries = computed(() => system.history.map((h) => h.netDown))
const upSeries = computed(() => system.history.map((h) => h.netUp))

// True once we have at least two samples to draw a line between.
const hasData = computed(() => system.history.length >= 2)

// Shared vertical scale so the down and up curves are directly
// comparable in the overlay — otherwise each sparkline would
// auto-scale to its own peak and the two lines would lie about their
// relative magnitude.
const sharedMax = computed(() => {
  let peak = 1
  for (const h of system.history) {
    if (h.netDown > peak) peak = h.netDown
    if (h.netUp > peak) peak = h.netUp
  }
  return peak
})

// Human-readable rate formatter — B/s, KB/s, MB/s, GB/s (binary units).
function formatRate(bytesPerSec: number): string {
  if (!Number.isFinite(bytesPerSec) || bytesPerSec <= 0) return '0 B/s'
  const units = ['B/s', 'KB/s', 'MB/s', 'GB/s']
  let value = bytesPerSec
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit += 1
  }
  const digits = value >= 100 || unit === 0 ? 0 : 1
  return `${value.toFixed(digits)} ${units[unit]}`
}

const downRate = computed(() => formatRate(system.netDownBytesPerSec))
const upRate = computed(() => formatRate(system.netUpBytesPerSec))
</script>

<template>
  <div class="cn-widget p-4 md:p-5">
    <div class="mb-4 flex items-center gap-2">
      <AppIcon name="Network" :size="16" class="text-[var(--cn-text-muted)]" />
      <h3 class="text-sm font-semibold text-[var(--cn-text)]">
        {{ t('dashboard.widgets.traffic.title') }}
      </h3>
    </div>

    <!-- Current rates: download (blue) + upload (emerald). -->
    <div class="flex items-center gap-4">
      <div class="min-w-0 flex-1">
        <div class="flex items-center gap-1 text-blue-500">
          <AppIcon name="ArrowDown" :size="14" />
          <span class="text-[10px] font-medium uppercase tracking-wider">
            {{ t('dashboard.widgets.traffic.down') }}
          </span>
        </div>
        <div class="mt-0.5 truncate text-lg font-semibold tabular-nums text-[var(--cn-text)]">
          {{ downRate }}
        </div>
      </div>
      <div class="min-w-0 flex-1">
        <div class="flex items-center gap-1 text-emerald-500">
          <AppIcon name="ArrowUp" :size="14" />
          <span class="text-[10px] font-medium uppercase tracking-wider">
            {{ t('dashboard.widgets.traffic.up') }}
          </span>
        </div>
        <div class="mt-0.5 truncate text-lg font-semibold tabular-nums text-[var(--cn-text)]">
          {{ upRate }}
        </div>
      </div>
    </div>

    <!-- Recent history graph — overlaid down + up sparklines. -->
    <div v-if="hasData" class="relative mt-3">
      <SparklineChart
        :data="downSeries"
        color="rgb(59 130 246)"
        :height="44"
        :max="sharedMax"
        :capacity="HISTORY_CAPACITY"
        class="absolute inset-0"
      />
      <SparklineChart
        :data="upSeries"
        color="rgb(16 185 129)"
        :height="44"
        :max="sharedMax"
        :capacity="HISTORY_CAPACITY"
      />
    </div>
    <div
      v-else
      class="mt-3 flex h-[44px] items-center justify-center text-xs text-[var(--cn-text-muted)]"
    >
      {{ t('dashboard.widgets.traffic.no_data') }}
    </div>
  </div>
</template>
