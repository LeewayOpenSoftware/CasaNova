<script setup lang="ts">
// NetworkWidget — down/up traffic speeds + a rolling sparkline.
//
// Reads real network throughput from the system store which polls
// GET /v1/sys/utilization every 2 seconds.

import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useSystemStore } from '@/stores/system'

const { t } = useI18n()
const system = useSystemStore()

// Current visible speeds from the real system store (bytes/s)
const currentDown = computed(() => system.netDownBytesPerSec / 1024) // KB/s
const currentUp = computed(() => system.netUpBytesPerSec / 1024) // KB/s

// Build sparkline buffers from the store's rolling history
const downSamples = computed(() => system.history.map((h) => h.netDown / 1024))
const upSamples = computed(() => system.history.map((h) => h.netUp / 1024))

// Fixed capacity for the x-axis: matches HISTORY_SIZE in the system
// store (60 samples at a 3s poll = ~3 minutes of data). Keeping this
// in lockstep with the store means the step width is constant instead
// of shrinking as the buffer fills.
const HISTORY_CAPACITY = 60

// Format bytes/s as a human-readable string.
function formatSpeed(kbs: number): string {
  if (kbs >= 1024) return `${(kbs / 1024).toFixed(1)} MB/s`
  return `${Math.round(kbs)} KB/s`
}

// Convert a sample buffer into an SVG polyline points string over a
// 100x40 viewbox.
//
// Previously this divided 100 by `samples.length - 1`, which made the
// step width shrink every time a new sample arrived. That pinned the
// newest value at x=100 and squeezed the whole curve from the right,
// producing the "graph compresses right-to-left" symptom — exactly
// the opposite of the rolling-window behavior a live network widget
// is supposed to show.
//
// Fix: derive the step from HISTORY_CAPACITY (constant) and
// right-align. When the buffer is half full the left portion of the
// chart is blank and new samples visibly push in from the right until
// the buffer saturates; once saturated, old samples fall off the left
// edge the way a classic rolling sparkline behaves.
function toPoints(samples: number[]): string {
  if (samples.length < 2) return ''
  const max = samples.reduce((a, b) => (a > b ? a : b), 1)
  const stepX = 100 / (HISTORY_CAPACITY - 1)
  const offset = 100 - (samples.length - 1) * stepX
  return samples
    .map((v, i) => {
      const x = offset + i * stepX
      const y = 40 - (v / max) * 36
      return `${x.toFixed(1)},${y.toFixed(1)}`
    })
    .join(' ')
}

const downPoints = computed(() => toPoints(downSamples.value))
const upPoints = computed(() => toPoints(upSamples.value))
</script>

<template>
  <div class="cn-widget p-5">
    <h3 class="mb-3 text-sm font-semibold text-[var(--cn-text)]">
      {{ t('dashboard.network') }}
    </h3>

    <div class="mb-3 flex items-center justify-between">
      <div class="flex items-center gap-2">
        <AppIcon name="ArrowDown" :size="16" class="text-blue-500" />
        <div>
          <div class="text-xs text-[var(--cn-text-muted)]">{{ t('dashboard.download') }}</div>
          <div class="text-sm font-semibold tabular-nums text-[var(--cn-text)]">
            {{ formatSpeed(currentDown) }}
          </div>
        </div>
      </div>
      <div class="flex items-center gap-2">
        <AppIcon name="ArrowUp" :size="16" class="text-emerald-500" />
        <div>
          <div class="text-xs text-[var(--cn-text-muted)]">{{ t('dashboard.upload') }}</div>
          <div class="text-sm font-semibold tabular-nums text-[var(--cn-text)]">
            {{ formatSpeed(currentUp) }}
          </div>
        </div>
      </div>
    </div>

    <svg viewBox="0 0 100 40" class="h-12 w-full" preserveAspectRatio="none">
      <line
        x1="0" y1="20" x2="100" y2="20"
        stroke="var(--cn-border)" stroke-width="0.5" stroke-dasharray="2 2"
      />
      <polyline
        :points="downPoints"
        fill="none" stroke="rgb(59 130 246)" stroke-width="1.5"
        stroke-linejoin="round" stroke-linecap="round"
      />
      <polyline
        :points="upPoints"
        fill="none" stroke="rgb(16 185 129)" stroke-width="1.5"
        stroke-linejoin="round" stroke-linecap="round"
      />
    </svg>
  </div>
</template>
