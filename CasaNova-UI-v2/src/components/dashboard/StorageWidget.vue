<script setup lang="ts">
// Storage widget — shows used/total + a progress bar, with a health
// badge. Wired to the system store which polls /v1/sys/utilization.

import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useSystemStore } from '@/stores/system'

const { t } = useI18n()
const router = useRouter()
const system = useSystemStore()

function openStorageSettings() {
  router.push('/app/settings/storage')
}

const usedBytes = computed(() => system.diskUsedBytes)
const totalBytes = computed(() => system.diskTotalBytes)
const percent = computed(() => totalBytes.value > 0 ? (usedBytes.value / totalBytes.value) * 100 : 0)
const health = computed<'healthy' | 'warning' | 'critical'>(() => {
  if (!system.diskHealthy) return 'critical'
  if (percent.value > 90) return 'warning'
  return 'healthy'
})

function formatBytes(bytes: number): string {
  const tb = bytes / 1024 ** 4
  if (tb >= 1) return `${tb.toFixed(2)} TB`
  const gb = bytes / 1024 ** 3
  return `${gb.toFixed(2)} GB`
}

const healthBadge = computed(() => {
  switch (health.value) {
    case 'healthy':
      return { label: t('dashboard.healthy'), color: 'bg-emerald-500/15 text-emerald-400 border-emerald-500/30' }
    case 'warning':
      return { label: t('dashboard.warning'), color: 'bg-amber-500/15 text-amber-400 border-amber-500/30' }
    case 'critical':
      return { label: t('dashboard.critical'), color: 'bg-red-500/15 text-red-400 border-red-500/30' }
  }
})
</script>

<template>
  <div class="cn-widget p-5">
    <div class="mb-4 flex items-center justify-between">
      <h3 class="text-sm font-semibold text-[var(--cn-text)]">
        {{ t('dashboard.storage') }}
      </h3>
      <button
        class="cn-icon-btn h-7 w-7"
        :aria-label="t('dashboard.storage_settings')"
        :title="t('dashboard.storage_settings')"
        @click="openStorageSettings"
      >
        <AppIcon name="Settings2" :size="16" />
      </button>
    </div>

    <div class="flex items-center gap-3">
      <div class="flex h-16 w-16 flex-shrink-0 items-center justify-center">
        <img
          src="/assets/brand/storage3.png"
          alt=""
          class="h-full w-full object-contain"
          aria-hidden="true"
        />
      </div>
      <div class="min-w-0 flex-1">
        <div class="mb-1 flex items-center gap-2">
          <span class="truncate text-sm font-medium text-[var(--cn-text)]">DATA</span>
          <span
            :class="[
              'inline-flex items-center rounded-full border px-2 py-0.5 text-[10px] font-medium',
              healthBadge.color,
            ]"
          >
            {{ healthBadge.label }}
          </span>
        </div>
        <div class="text-xs text-[var(--cn-text-muted)]">
          {{ t('dashboard.used') }}: {{ formatBytes(usedBytes) }}
        </div>
        <div class="text-xs text-[var(--cn-text-muted)]">
          {{ t('dashboard.total') }}: {{ formatBytes(totalBytes) }}
        </div>
      </div>
    </div>

    <div class="mt-4 h-2 w-full overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
      <div
        class="h-full rounded-full bg-blue-500 transition-[width] duration-700 ease-out"
        :style="{ width: `${percent}%` }"
      />
    </div>
  </div>
</template>
