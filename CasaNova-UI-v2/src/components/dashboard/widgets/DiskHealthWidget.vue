<script setup lang="ts">
// Disk health (SMART) widget — one compact row per physical disk with
// a SMART health pill and temperature. Data comes from the storage API
// (listDisks), which runs smartctl inline on the backend and embeds
// health + temperature in each DiskEntry.
//
// Polls every ~60s (SMART data is slow-moving, so a tight loop would
// only add noise) and cleans up the timer on unmount so navigating
// away from the dashboard doesn't leak intervals.

import { skipWhenHidden } from '@/composables/skipWhenHidden'
import { ref, onMounted, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import { listDisks, type DiskEntry, type SmartHealth } from '@/api/storage'

const { t } = useI18n()

const disks = ref<DiskEntry[]>([])
const loading = ref(true)
const errored = ref(false)
let timer: number | null = null

async function loadDisks() {
  try {
    disks.value = await listDisks()
    errored.value = false
  } catch {
    // Backend unreachable or SMART unavailable — surface a graceful
    // error state rather than leaving a stale/blank widget.
    errored.value = true
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void loadDisks()
  timer = window.setInterval(skipWhenHidden(() => {
    void loadDisks()
  }), 60_000)
})

onBeforeUnmount(() => {
  if (timer !== null) window.clearInterval(timer)
})

// Collapse the backend's four-state health into the three visual
// states the pill shows: ok (emerald), failing (red), unknown (muted).
type PillState = 'ok' | 'failing' | 'unknown'

function pillFor(health: SmartHealth): { state: PillState; classes: string; labelKey: string } {
  switch (health) {
    case 'good':
      return {
        state: 'ok',
        classes: 'bg-emerald-500/15 text-emerald-500 border-emerald-500/30',
        labelKey: 'dashboard.widgets.diskhealth.ok',
      }
    case 'warning':
    case 'critical':
      return {
        state: 'failing',
        classes: 'bg-red-500/15 text-red-500 border-red-500/30',
        labelKey: 'dashboard.widgets.diskhealth.failing',
      }
    default:
      return {
        state: 'unknown',
        classes: 'bg-[var(--cn-surface-hover)] text-[var(--cn-text-muted)] border-[var(--cn-border)]',
        labelKey: 'dashboard.widgets.diskhealth.unknown',
      }
  }
}

// Prefer the model name; fall back to the device path when the drive
// reports no model string (some USB enclosures do this).
function diskLabel(d: DiskEntry): string {
  return d.model?.trim() || d.device
}
</script>

<template>
  <div class="cn-widget p-4 md:p-5">
    <div class="mb-3 flex items-center gap-2">
      <AppIcon name="HardDrive" :size="16" class="text-[var(--cn-text-muted)]" />
      <h3 class="text-sm font-semibold text-[var(--cn-text)]">
        {{ t('dashboard.widgets.diskhealth.title') }}
      </h3>
    </div>

    <!-- Loading -->
    <div
      v-if="loading"
      class="py-3 text-center text-xs text-[var(--cn-text-muted)]"
    >
      {{ t('dashboard.widgets.diskhealth.loading') }}
    </div>

    <!-- Error / SMART unavailable -->
    <div
      v-else-if="errored"
      class="py-3 text-center text-xs text-[var(--cn-text-muted)]"
    >
      {{ t('dashboard.widgets.diskhealth.unavailable') }}
    </div>

    <!-- Empty -->
    <div
      v-else-if="disks.length === 0"
      class="py-3 text-center text-xs text-[var(--cn-text-muted)]"
    >
      {{ t('dashboard.widgets.diskhealth.empty') }}
    </div>

    <!-- Disk rows -->
    <ul v-else class="flex flex-col gap-1">
      <li
        v-for="d in disks"
        :key="d.device"
        class="flex items-center justify-between gap-2 rounded-md px-1 py-1 text-xs"
      >
        <span
          class="min-w-0 flex-1 truncate text-[var(--cn-text)]"
          :title="diskLabel(d)"
        >
          {{ diskLabel(d) }}
        </span>

        <div class="flex shrink-0 items-center gap-2">
          <span
            v-if="d.temperature !== null"
            class="font-mono tabular-nums text-[var(--cn-text-muted)]"
          >
            {{ d.temperature }}°C
          </span>
          <span
            :class="[
              'inline-flex items-center rounded-full border px-2 py-0.5 text-[10px] font-medium',
              pillFor(d.smartHealth).classes,
            ]"
          >
            {{ t(pillFor(d.smartHealth).labelKey) }}
          </span>
        </div>
      </li>
    </ul>
  </div>
</template>
