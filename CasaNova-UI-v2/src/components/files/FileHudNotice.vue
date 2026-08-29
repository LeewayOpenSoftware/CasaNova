<script setup lang="ts">
// FileHudNotice — a compact heads-up display in the file manager's top
// strip. Surfaces the most recent UNREAD notification inline (level dot +
// title) so it catches the eye without opening the drawer; clicking it
// opens the full notification sidebar. Renders nothing when all caught up,
// so the strip stays clean at rest.

import { computed } from 'vue'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useNotificationsStore } from '@/stores/notifications'

const notifications = useNotificationsStore()

// Most-recent unread; the store keeps items newest-first.
const latest = computed(() => notifications.unreadItems[0] ?? null)
const extra = computed(() => Math.max(0, notifications.unreadCount - 1))

const DOT: Record<string, string> = {
  info: 'bg-blue-500',
  success: 'bg-emerald-500',
  warning: 'bg-amber-500',
  error: 'bg-red-500',
}
const ICON: Record<string, string> = {
  info: 'Info',
  success: 'CheckCircle2',
  warning: 'AlertTriangle',
  error: 'AlertOctagon',
}
</script>

<template>
  <button
    v-if="latest"
    type="button"
    class="flex max-w-[22rem] items-center gap-2 rounded-full border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-3 py-1 text-xs text-[var(--cn-text)] transition-colors hover:border-[var(--cn-text-muted)]"
    :title="`${latest.title} — ${latest.message}`"
    @click="notifications.toggleSidebar()"
  >
    <span class="relative flex h-2 w-2 shrink-0">
      <span
        class="absolute inline-flex h-full w-full animate-ping rounded-full opacity-60"
        :class="DOT[latest.level] || 'bg-blue-500'"
      />
      <span class="relative inline-flex h-2 w-2 rounded-full" :class="DOT[latest.level] || 'bg-blue-500'" />
    </span>
    <AppIcon :name="ICON[latest.level] || 'Bell'" :size="13" class="shrink-0 text-[var(--cn-text-muted)]" />
    <span class="truncate font-medium">{{ latest.title }}</span>
    <span
      v-if="extra > 0"
      class="shrink-0 rounded-full bg-[var(--cn-surface)] px-1.5 text-[10px] font-semibold text-[var(--cn-text-muted)]"
    >+{{ extra }}</span>
  </button>
</template>
