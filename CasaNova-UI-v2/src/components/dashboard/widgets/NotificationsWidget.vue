<script setup lang="ts">
// Notifications widget — recent activity feed for the dashboard sidebar.
//
// Reads the notifications store (list + unread count) reactively. The
// store owns its own 30s poll (started by AppLayout), so this widget
// never starts a timer of its own — it just renders the latest few
// items and a "Ver todas" affordance that opens the sidebar drawer.

import { computed } from 'vue'
import { storeToRefs } from 'pinia'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useNotificationsStore } from '@/stores/notifications'
import type { NotificationLevel } from '@/api/notifications'

const { t } = useI18n()
const store = useNotificationsStore()
const { items, unreadCount } = storeToRefs(store)

// Latest up to 3 — the backend returns newest-first, so a head slice is
// enough. Kept tiny so the widget stays compact in the narrow column.
const recent = computed(() => items.value.slice(0, 3))

// Level → dot color. Category can also carry a severity hint but the
// level is the canonical signal, so we map off that.
function dotClass(level: NotificationLevel): string {
  switch (level) {
    case 'error':
      return 'bg-red-500'
    case 'warning':
      return 'bg-amber-500'
    case 'success':
      return 'bg-emerald-500'
    case 'info':
    default:
      return 'bg-blue-500'
  }
}

// Simple relative-ish formatter: "hace Xm" / "hace Xh" / "hace Xd".
// Returns null for missing/unparseable timestamps so the caller can omit it.
function relativeTime(iso: string | undefined): string | null {
  if (!iso) return null
  const then = Date.parse(iso)
  if (Number.isNaN(then)) return null
  const diffMs = Date.now() - then
  if (diffMs < 0) return t('dashboard.widgets.notifications.now')
  const mins = Math.floor(diffMs / 60_000)
  if (mins < 1) return t('dashboard.widgets.notifications.now')
  if (mins < 60) return t('dashboard.widgets.notifications.minutes_ago', { n: mins })
  const hours = Math.floor(mins / 60)
  if (hours < 24) return t('dashboard.widgets.notifications.hours_ago', { n: hours })
  const days = Math.floor(hours / 24)
  return t('dashboard.widgets.notifications.days_ago', { n: days })
}

function openAll() {
  store.toggleSidebar()
}
</script>

<template>
  <div class="cn-widget p-4 md:p-5">
    <div class="mb-3 flex items-center justify-between">
      <div class="flex items-center gap-2">
        <AppIcon name="Bell" :size="16" class="text-[var(--cn-text-muted)]" />
        <h3 class="text-sm font-semibold text-[var(--cn-text)]">
          {{ t('dashboard.widgets.notifications.title') }}
        </h3>
      </div>
      <span
        v-if="unreadCount > 0"
        class="inline-flex min-w-[1.25rem] items-center justify-center rounded-full bg-blue-500/15 px-1.5 py-0.5 text-[10px] font-semibold tabular-nums text-blue-500"
        :aria-label="t('dashboard.widgets.notifications.unread', { n: unreadCount })"
      >
        {{ unreadCount }}
      </span>
    </div>

    <!-- Empty state -->
    <p
      v-if="recent.length === 0"
      class="py-2 text-xs text-[var(--cn-text-muted)]"
    >
      {{ t('dashboard.widgets.notifications.empty') }}
    </p>

    <!-- Latest notifications -->
    <ul v-else class="flex flex-col gap-2">
      <li
        v-for="n in recent"
        :key="n.id"
        class="flex items-start gap-2"
      >
        <span
          :class="['mt-1.5 h-2 w-2 shrink-0 rounded-full', dotClass(n.level)]"
          aria-hidden="true"
        />
        <div class="min-w-0 flex-1">
          <div class="truncate text-xs font-medium text-[var(--cn-text)]">
            {{ n.title }}
          </div>
          <div
            v-if="relativeTime(n.created_at)"
            class="text-[10px] text-[var(--cn-text-muted)]"
          >
            {{ relativeTime(n.created_at) }}
          </div>
        </div>
      </li>
    </ul>

    <button
      type="button"
      class="mt-3 w-full rounded-md px-2 py-1.5 text-xs font-medium text-blue-500 transition-colors hover:bg-[var(--cn-surface-hover)]"
      @click="openAll"
    >
      {{ t('dashboard.widgets.notifications.view_all') }}
    </button>
  </div>
</template>
