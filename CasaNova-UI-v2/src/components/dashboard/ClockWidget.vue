<script setup lang="ts">
// Clock widget — large time display + long-form date.
//
// Updates every second via setInterval, cleaned up on unmount so
// the widget doesn't leak timers when the dashboard unmounts during
// navigation. Locale comes from vue-i18n so the time format matches
// the user's chosen language.

import { ref, onMounted, onBeforeUnmount, computed } from 'vue'
import { useI18n } from 'vue-i18n'

const { locale } = useI18n()
const now = ref(new Date())
let timer: number | null = null

onMounted(() => {
  timer = window.setInterval(() => {
    now.value = new Date()
  }, 1000)
})

onBeforeUnmount(() => {
  if (timer !== null) window.clearInterval(timer)
})

// Time as HH:MM — seconds deliberately omitted to avoid the ticking
// eye-drawer effect on an always-visible widget.
const timeString = computed(() =>
  now.value.toLocaleTimeString(locale.value.replace('_', '-'), {
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }),
)

// Long-form date ("Sunday, 5 April 2026") — gives the widget enough
// weight to balance the other two widgets in the column.
const dateString = computed(() =>
  now.value.toLocaleDateString(locale.value.replace('_', '-'), {
    weekday: 'long',
    day: 'numeric',
    month: 'long',
    year: 'numeric',
  }),
)
</script>

<template>
  <div class="cn-widget p-4 md:p-5">
    <div class="text-4xl font-light tabular-nums tracking-tight text-[var(--cn-text)] md:text-5xl">
      {{ timeString }}
    </div>
    <div class="mt-1 text-xs text-[var(--cn-text-muted)] md:text-sm">
      {{ dateString }}
    </div>
  </div>
</template>
