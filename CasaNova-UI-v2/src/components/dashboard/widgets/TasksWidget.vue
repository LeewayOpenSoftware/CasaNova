<script setup lang="ts">
// Scheduled tasks widget — shows the count of cron jobs plus a preview
// of the first few, each with its raw cron schedule expression in a
// monospace pill. We deliberately do NOT compute the next run time
// (no cron-parsing lib is available) — the schedule expression is
// enough signal for an at-a-glance dashboard widget.
//
// Data comes from GET /v1/sys/cron via the cron API. The call is
// wrapped in try/catch so an empty or unauthorized endpoint degrades
// gracefully to the empty state instead of breaking the dashboard.
// A light ~60s poll keeps the list fresh; the timer is cleaned up on
// unmount so navigation away doesn't leak it.

import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import * as cronApi from '@/api/cron'
import type { CronJob } from '@/api/cron'

const { t } = useI18n()

const jobs = ref<CronJob[]>([])
let timer: number | null = null

async function load(): Promise<void> {
  try {
    jobs.value = await cronApi.getJobs()
  } catch {
    // Endpoint may be empty or unauthorized-safe — keep the last known
    // list rather than blanking the widget on a transient failure.
  }
}

onMounted(() => {
  void load()
  timer = window.setInterval(() => {
    void load()
  }, 60_000)
})

onBeforeUnmount(() => {
  if (timer !== null) window.clearInterval(timer)
})

const count = computed(() => jobs.value.length)

// Preview up to 3 jobs. Prefer the human description, falling back to
// the raw command (truncated) when no description is set.
const preview = computed(() => jobs.value.slice(0, 3))

function labelFor(job: CronJob): string {
  const text = job.description?.trim() || job.command?.trim() || ''
  return text.length > 42 ? text.slice(0, 41) + '…' : text
}
</script>

<template>
  <div class="cn-widget p-4 md:p-5">
    <div class="mb-3 flex items-center justify-between">
      <div class="flex items-center gap-2">
        <AppIcon name="Clock" :size="16" class="text-[var(--cn-text-muted)]" />
        <h3 class="text-sm font-semibold text-[var(--cn-text)]">
          {{ t('dashboard.widgets.tasks.title') }}
        </h3>
      </div>
      <span
        v-if="count > 0"
        class="inline-flex items-center rounded-full border border-[var(--cn-border)] px-2 py-0.5 text-[10px] font-medium text-[var(--cn-text-muted)]"
      >
        {{ t('dashboard.widgets.tasks.count', { count }) }}
      </span>
    </div>

    <ul v-if="preview.length > 0" class="flex flex-col gap-2">
      <li
        v-for="job in preview"
        :key="job.id"
        class="flex items-center justify-between gap-2 rounded-md px-1 py-0.5 text-xs"
      >
        <span class="min-w-0 flex-1 truncate text-[var(--cn-text)]">
          {{ labelFor(job) }}
        </span>
        <span
          class="shrink-0 rounded bg-[var(--cn-surface-hover)] px-1.5 py-0.5 font-mono text-[10px] tabular-nums text-[var(--cn-text-muted)]"
        >
          {{ job.schedule }}
        </span>
      </li>
    </ul>

    <div v-else class="py-3 text-center text-xs text-[var(--cn-text-muted)]">
      {{ t('dashboard.widgets.tasks.empty') }}
    </div>
  </div>
</template>
