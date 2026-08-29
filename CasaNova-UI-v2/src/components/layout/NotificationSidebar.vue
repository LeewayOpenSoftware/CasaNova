<script setup lang="ts">
// NotificationSidebar — slide-in right-side drawer showing the feed of
// recent notifications from the backend. Bound to the notifications Pinia
// store so every change (new item arrives via poll, user marks read,
// user deletes) re-renders reactively.
//
// Uses PrimeVue's Drawer (formerly Sidebar) in right-side mode. The
// visibility binding is two-way against the store's `sidebarOpen` state
// so closing via the backdrop, Escape key, or the X button all flow
// through the same code path.
//
// Empty state, loading state, and error state all render distinct
// messages so the user never sees a dead panel.

import { computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import dayjs from 'dayjs'
import relativeTime from 'dayjs/plugin/relativeTime'
import Drawer from 'primevue/drawer'
import Button from 'primevue/button'
import Select from 'primevue/select'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useNotificationsStore } from '@/stores/notifications'
import type { Notification, NotificationLevel, NotificationCategory } from '@/api/notifications'

dayjs.extend(relativeTime)

const { t } = useI18n()
const router = useRouter()
const store = useNotificationsStore()

// Two-way binding helper — PrimeVue Drawer expects a plain ref-like
// `visible`, so we proxy it through a computed get/set that reads and
// writes the store's `sidebarOpen`.
const visible = computed({
  get: () => store.sidebarOpen,
  set: (v) => {
    if (!v) store.closeSidebar()
  },
})

// Icon/color mapping per level. Keeping this as a plain lookup object
// rather than a switch in the template means the level → visual
// treatment is in one place and easy to extend.
const levelStyle: Record<
  NotificationLevel,
  { icon: string; color: string; bg: string }
> = {
  info: {
    icon: 'Info',
    color: 'text-blue-600 dark:text-blue-500',
    bg: 'bg-blue-500/10',
  },
  success: {
    icon: 'CheckCircle2',
    color: 'text-emerald-600 dark:text-emerald-400',
    bg: 'bg-emerald-50 dark:bg-emerald-950',
  },
  warning: {
    icon: 'AlertTriangle',
    color: 'text-amber-600 dark:text-amber-400',
    bg: 'bg-amber-50 dark:bg-amber-950',
  },
  error: {
    icon: 'AlertOctagon',
    color: 'text-red-600 dark:text-red-400',
    bg: 'bg-red-50 dark:bg-red-950',
  },
}

function relative(iso: string): string {
  return dayjs(iso).fromNow()
}

// Category filter — a single compact dropdown instead of the old
// horizontally-scrollable tab row (which needed a sideways scrollbar to
// reach half the categories). 'all' is a sentinel code: PrimeVue Select
// treats a null model as "no selection", so we bridge 'all' ⇄ the store's
// null filter. Counts are computed from the raw list so users see
// "12 app events, 3 security events" before filtering.
interface CategoryOption {
  code: 'all' | NotificationCategory
  labelKey: string
  icon: string
}
const categoryOptions: CategoryOption[] = [
  { code: 'all', labelKey: 'notifications.filter.all', icon: 'Layers' },
  { code: 'app', labelKey: 'notifications.filter.app', icon: 'Container' },
  { code: 'storage', labelKey: 'notifications.filter.storage', icon: 'HardDrive' },
  { code: 'system', labelKey: 'notifications.filter.system', icon: 'Cpu' },
  { code: 'update', labelKey: 'notifications.filter.update', icon: 'Download' },
  { code: 'security', labelKey: 'notifications.filter.security', icon: 'Shield' },
  { code: 'backup', labelKey: 'notifications.filter.backup', icon: 'CloudUpload' },
]
const categoryOptionsLocalized = computed(() =>
  categoryOptions.map((o) => ({ ...o, label: t(o.labelKey) })),
)
const optionByCode = (code: string) =>
  categoryOptions.find((o) => o.code === code) ?? categoryOptions[0]

const categoryModel = computed({
  get: () => store.categoryFilter ?? 'all',
  set: (code: 'all' | NotificationCategory) =>
    store.setCategoryFilter(code === 'all' ? null : code),
})

// Always open pinned to "Todas": a filter left behind by the previous
// visit made the panel look empty ("Estás al día" under a stale category
// while other categories had items).
watch(
  () => store.sidebarOpen,
  (open) => {
    if (open) store.setCategoryFilter(null)
  },
)

function countFor(code: string): number {
  if (code === 'all') return store.items.length
  return store.items.filter((n) => n.category === code).length
}

function onItemClick(n: Notification) {
  // Mark-read is fire-and-forget: navigation must NOT wait on it. Previously
  // this awaited markOneRead, so a slow/hung bus (up to the 10s client
  // timeout) froze the click before the deep-link fired.
  if (!n.read) {
    void store.markOneRead(n.id)
  }
  // Deep-link: internal routes (/app/...) navigate in-app; external URLs
  // open in a new tab. Close the sidebar on internal navigation so the
  // user lands cleanly on the target page.
  if (n.link) {
    if (n.link.startsWith('/')) {
      store.closeSidebar()
      router.push(n.link)
    } else {
      window.open(n.link, '_blank', 'noopener,noreferrer')
    }
  }
}

async function onDismissAll() {
  await store.dismissAll()
}
</script>

<template>
  <Drawer v-model:visible="visible" position="right" class="w-full max-w-md">
    <template #header>
      <div class="flex w-full items-center justify-between">
        <div class="flex items-center gap-2">
          <h2 class="text-lg font-semibold">{{ t('notifications.title') }}</h2>
          <span
            v-if="store.unreadCount > 0"
            class="rounded-full bg-blue-500/15 px-2 py-0.5 text-xs font-medium text-blue-500"
          >
            {{ store.unreadCount }}
          </span>
        </div>
        <Button
          v-if="store.items.length > 0"
          text
          size="small"
          severity="secondary"
          @click="onDismissAll"
        >
          <AppIcon name="Trash2" :size="14" class="mr-1" />
          {{ t('notifications.dismiss_all') }}
        </Button>
      </div>
    </template>

    <!-- Category filter — one compact dropdown (replaces the scrollable
         tab row). Opens pinned to "All" (see the sidebarOpen watcher);
         picking a category filters `filteredItems` exactly as before. -->
    <div
      class="sticky top-0 z-10 -mx-4 mb-2 border-b border-[var(--cn-border)] bg-[var(--cn-surface)] px-4 pb-2 pt-1"
    >
      <Select
        v-model="categoryModel"
        :options="categoryOptionsLocalized"
        option-label="label"
        option-value="code"
        class="w-52"
        size="small"
      >
        <template #value="{ value }">
          <div v-if="value" class="inline-flex items-center gap-1.5 text-xs">
            <AppIcon :name="optionByCode(value).icon" :size="13" />
            <span>{{ t(optionByCode(value).labelKey) }}</span>
            <span
              v-if="countFor(value) > 0"
              class="rounded-full bg-[var(--cn-surface-hover)] px-1.5 text-[10px] text-[var(--cn-text-muted)]"
            >
              {{ countFor(value) }}
            </span>
          </div>
        </template>
        <template #option="{ option }">
          <div class="inline-flex w-full items-center gap-1.5 text-xs">
            <AppIcon :name="option.icon" :size="13" />
            <span>{{ option.label }}</span>
            <span
              v-if="countFor(option.code) > 0"
              class="ml-auto rounded-full bg-[var(--cn-surface-hover)] px-1.5 text-[10px] text-[var(--cn-text-muted)]"
            >
              {{ countFor(option.code) }}
            </span>
          </div>
        </template>
      </Select>
    </div>

    <!-- Loading state: show on the very first fetch. Subsequent polls are
         silent so the list doesn't flash. -->
    <div
      v-if="store.loading && store.items.length === 0"
      class="flex h-32 items-center justify-center text-slate-500"
    >
      {{ t('common.loading') }}
    </div>

    <!-- Error state: show only when there's no cached data to show. When
         cached items exist, the error is shown as a slim banner above the
         list so the user still sees their notifications. -->
    <div
      v-else-if="store.lastError && store.items.length === 0"
      class="rounded border border-red-200 bg-red-50 p-4 text-sm text-red-700 dark:border-red-900 dark:bg-red-950 dark:text-red-300"
    >
      {{ store.lastError }}
      <div class="mt-2">
        <Button size="small" severity="secondary" @click="store.refresh()">
          {{ t('notifications.retry') }}
        </Button>
      </div>
    </div>

    <!-- Empty state: friendly copy, zero items (or empty filter). -->
    <div
      v-else-if="store.filteredItems.length === 0"
      class="flex h-48 flex-col items-center justify-center text-center"
    >
      <div class="mb-2 text-4xl">✨</div>
      <p class="text-sm font-medium">{{ t('notifications.empty_title') }}</p>
      <p class="text-xs text-slate-500">
        {{ store.categoryFilter === null
            ? t('notifications.empty_subtitle')
            : t('notifications.empty_filter') }}
      </p>
    </div>

    <!-- Slim error banner when we have cached items but the last refresh failed. -->
    <div
      v-if="store.lastError && store.items.length > 0"
      class="mb-2 flex items-center justify-between rounded border border-red-200 bg-red-50 px-3 py-1.5 text-xs text-red-700 dark:border-red-900 dark:bg-red-950 dark:text-red-300"
    >
      <span>{{ store.lastError }}</span>
      <Button text size="small" severity="secondary" @click="store.refresh()">
        {{ t('notifications.retry') }}
      </Button>
    </div>

    <!-- List of notifications, newest first (the backend returns them in
         this order already). Read vs unread is indicated by background
         opacity; level drives the icon + accent color. -->
    <ul v-if="store.filteredItems.length > 0" class="divide-y divide-slate-200 dark:divide-slate-800">
      <li
        v-for="n in store.filteredItems"
        :key="n.id"
        :class="[
          'group flex cursor-pointer gap-3 p-3 transition-colors',
          n.read
            ? 'opacity-70 hover:opacity-100 hover:bg-slate-50 dark:hover:bg-slate-800'
            : 'bg-slate-50 hover:bg-slate-100 dark:bg-slate-900 dark:hover:bg-slate-800',
        ]"
        @click="onItemClick(n)"
      >
        <div :class="['flex h-8 w-8 flex-shrink-0 items-center justify-center rounded-full', levelStyle[n.level].bg]">
          <AppIcon :name="levelStyle[n.level].icon" :size="16" :class="levelStyle[n.level].color" />
        </div>
        <div class="min-w-0 flex-1">
          <div class="flex items-start justify-between gap-2">
            <p class="truncate text-sm font-semibold">{{ n.title }}</p>
            <span class="flex-shrink-0 text-xs text-slate-500">{{ relative(n.created_at) }}</span>
          </div>
          <p class="mt-0.5 text-sm text-slate-600 dark:text-slate-400">{{ n.message }}</p>
        </div>
        <button
          class="text-[var(--cn-text-muted)] opacity-60 transition-opacity hover:text-red-500 hover:opacity-100"
          :aria-label="t('notifications.delete')"
          @click.stop="store.removeOne(n.id)"
        >
          <AppIcon name="Trash2" :size="16" />
        </button>
      </li>
    </ul>
  </Drawer>
</template>
