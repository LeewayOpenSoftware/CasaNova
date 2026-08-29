<script setup lang="ts">
// SystemStatsPanel — 2x2 grid of sparkline mini-charts for CPU,
// memory, network upload, and network download.  Reads from the
// system store's rolling history buffer so it animates in real time
// as the poll ticks.

import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useSystemStore } from '@/stores/system'
import SparklineChart from './SparklineChart.vue'

const { t } = useI18n()
const system = useSystemStore()

// Pluck per-metric arrays out of the history buffer.  Each computed
// re-evaluates whenever `system.history` changes (push/shift).
const cpuData = computed(() => system.history.map((e) => e.cpu))
const memData = computed(() => system.history.map((e) => e.memPercent))
const netUpData = computed(() => system.history.map((e) => e.netUp))
const netDownData = computed(() => system.history.map((e) => e.netDown))

// Format bytes/s into a human-readable string with adaptive units.
function formatBytesPerSec(bps: number): string {
  if (bps >= 1024 * 1024) return `${(bps / (1024 * 1024)).toFixed(1)} MB/s`
  if (bps >= 1024) return `${(bps / 1024).toFixed(1)} KB/s`
  return `${Math.round(bps)} B/s`
}

// Current values for the large text readouts.
const currentCpu = computed(() => `${system.cpuPercent}%`)
const currentMem = computed(() => `${system.ramPercent}%`)
const currentUp = computed(() => formatBytesPerSec(system.netUpBytesPerSec))
const currentDown = computed(() => formatBytesPerSec(system.netDownBytesPerSec))
</script>

<template>
  <div class="cn-widget p-4 md:p-5">
    <h3 class="mb-3 text-sm font-semibold text-[var(--cn-text)]">
      {{ t('dashboard.system_stats') }}
    </h3>

    <div class="grid grid-cols-2 gap-3">
      <!-- CPU -->
      <div class="flex flex-col gap-1">
        <div class="flex items-baseline justify-between">
          <span class="text-xs text-[var(--cn-text-muted)]">{{ t('dashboard.cpu_usage') }}</span>
          <span class="text-sm font-semibold tabular-nums text-[var(--cn-text)]">{{ currentCpu }}</span>
        </div>
        <SparklineChart
          :data="cpuData"
          color="rgb(59 130 246)"
          :max="100"
          :height="36"
          :capacity="60"
        />
      </div>

      <!-- Memory -->
      <div class="flex flex-col gap-1">
        <div class="flex items-baseline justify-between">
          <span class="text-xs text-[var(--cn-text-muted)]">{{ t('dashboard.memory_usage') }}</span>
          <span class="text-sm font-semibold tabular-nums text-[var(--cn-text)]">{{ currentMem }}</span>
        </div>
        <SparklineChart
          :data="memData"
          color="rgb(168 85 247)"
          :max="100"
          :height="36"
          :capacity="60"
        />
      </div>

      <!-- Network Upload -->
      <div class="flex flex-col gap-1">
        <div class="flex items-baseline justify-between">
          <span class="text-xs text-[var(--cn-text-muted)]">{{ t('dashboard.network_up') }}</span>
          <span class="text-sm font-semibold tabular-nums text-[var(--cn-text)]">{{ currentUp }}</span>
        </div>
        <SparklineChart
          :data="netUpData"
          color="rgb(34 197 94)"
          :height="36"
          :capacity="60"
        />
      </div>

      <!-- Network Download -->
      <div class="flex flex-col gap-1">
        <div class="flex items-baseline justify-between">
          <span class="text-xs text-[var(--cn-text-muted)]">{{ t('dashboard.network_down') }}</span>
          <span class="text-sm font-semibold tabular-nums text-[var(--cn-text)]">{{ currentDown }}</span>
        </div>
        <SparklineChart
          :data="netDownData"
          color="rgb(249 115 22)"
          :height="36"
          :capacity="60"
        />
      </div>
    </div>
  </div>
</template>
