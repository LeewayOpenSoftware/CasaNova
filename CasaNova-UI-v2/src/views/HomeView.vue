<script setup lang="ts">
// HomeView — the dashboard. Stage 3 + polish pass.
//
// Layout:
//   ┌──────────────────────────────────────────────────────────┐
//   │                      top bar (AppLayout)                  │
//   ├──────────────┬────────────────────────────────────────────┤
//   │  Clock       │  Tips carousel  (cn-glass variant)         │
//   │              ├────────────────────────────────────────────┤
//   │  System      │  Search                                    │
//   │  state       ├────────────────────────────────────────────┤
//   │              │                                            │
//   │  Storage     │  App grid                                  │
//   │              │                                            │
//   │  Network     │                                            │
//   ├──────────────┴────────────────────────────────────────────┤
//   │                 CasaNova footer wordmark                   │
//   └────────────────────────────────────────────────────────────┘

import { onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import MorphIcon from '@/components/ui/MorphIcon.vue'
import YskCards from '@/components/dashboard/YskCards.vue'
import SearchBar from '@/components/dashboard/SearchBar.vue'
import AppGrid from '@/components/dashboard/AppGrid.vue'
import TipsCarousel from '@/components/dashboard/TipsCarousel.vue'
import { WIDGET_REGISTRY } from '@/components/dashboard/widgets/registry'
import { useWidgetLayoutStore } from '@/stores/widgetLayout'
import { useUserPrefs } from '@/composables/useUserPrefs'
import { useSystemStore } from '@/stores/system'

const { t } = useI18n()
const { showTips } = useUserPrefs()
// Dashboard widget layout (order / visibility / edit mode), persisted per user.
const wl = useWidgetLayoutStore()

// Wave 34-Y: StorageWidget and NetworkWidget read from the system
// store, but nothing on HomeView kicks off the polling loop that
// populates it. SystemStateWidget does call `startPolling()` on
// mount, but it's not rendered here — so on a fresh dashboard load
// the storage gauge and network graph sat at zero until the user
// navigated away and back, or opened a page that happened to mount
// SystemStateWidget. Kick the store here so the dashboard lights
// up immediately. `startPolling` is idempotent, so overlap with
// other views that also call it is harmless.
const system = useSystemStore()
onMounted(() => {
  system.startPolling()
})
</script>

<template>
  <div class="mx-auto flex min-h-full max-w-[1400px] flex-col p-4 sm:p-5 md:p-6">
    <!-- Grid breakpoints tuned to match CasaOS's density on narrow
         windows. We engage the two-column layout at `sm` (640px) with
         a compact 220px aside, step up to 260px at `md` (768px) and
         300px at `lg` (1024px). Below sm it collapses to single
         column for phones. This keeps the dashboard readable + dense
         across the full desktop/tablet/phone range, instead of
         jumping from "mobile full-width" to "desktop roomy" with
         nothing in between. -->
    <div class="grid flex-1 grid-cols-1 gap-4 sm:grid-cols-[220px_minmax(0,1fr)] md:grid-cols-[260px_minmax(0,1fr)] md:gap-5 lg:grid-cols-[300px_minmax(0,1fr)] lg:gap-6">
      <!-- Left widget column — four widgets stacking vertically. On
           the narrowest screens (mobile) the max-w cap prevents the
           widgets from ballooning to full viewport width, which is
           what was causing the oversized gauges vs CasaOS. -->
      <aside class="mx-auto flex w-full max-w-sm flex-col gap-3 sm:max-w-none md:gap-4">
        <!-- Widgets render dynamically from the layout store. In edit mode
             each gets a move-up/down + hide overlay and its own interactions
             are frozen so clicks reorder instead of triggering the widget. -->
        <div v-for="(id, idx) in wl.visible" :key="id" class="relative">
          <div
            v-if="wl.editMode"
            class="absolute right-1.5 top-1.5 z-20 flex items-center gap-0.5 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface)] p-0.5 shadow-md"
          >
            <button
              type="button" :disabled="idx === 0"
              class="flex h-6 w-6 items-center justify-center rounded text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)] disabled:opacity-30"
              :aria-label="t('dashboard.widgets.move_up')" @click="wl.move(id, -1)"
            ><AppIcon name="ChevronUp" :size="15" /></button>
            <button
              type="button" :disabled="idx === wl.visible.length - 1"
              class="flex h-6 w-6 items-center justify-center rounded text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)] disabled:opacity-30"
              :aria-label="t('dashboard.widgets.move_down')" @click="wl.move(id, 1)"
            ><AppIcon name="ChevronDown" :size="15" /></button>
            <button
              type="button"
              class="flex h-6 w-6 items-center justify-center rounded text-red-500 hover:bg-red-500/10"
              :aria-label="t('dashboard.widgets.hide')" @click="wl.hide(id)"
            ><AppIcon name="X" :size="15" /></button>
          </div>
          <div :class="wl.editMode ? 'pointer-events-none select-none rounded-2xl ring-2 ring-blue-500/40' : ''">
            <component :is="WIDGET_REGISTRY[id].component" />
          </div>
        </div>

        <!-- YSK cards render here so the onboarding hints sit next to
             the live system stats. Renders nothing when empty. -->
        <YskCards />

        <!-- Edit toggle + add-widget picker -->
        <button
          type="button"
          class="flex items-center justify-center gap-2 rounded-xl border border-[var(--cn-border)] px-3 py-2 text-sm text-[var(--cn-text-muted)] transition-colors hover:border-[var(--cn-text)] hover:text-[var(--cn-text)]"
          :class="wl.editMode ? 'border-blue-500 bg-blue-500/10 text-blue-500' : ''"
          @click="wl.toggleEdit()"
        >
          <MorphIcon :name="wl.editMode ? 'Check' : 'LayoutGrid'" :size="15" />
          {{ wl.editMode ? t('dashboard.widgets.done') : t('dashboard.widgets.edit') }}
        </button>

        <div v-if="wl.editMode" class="cn-widget flex flex-col gap-1.5 p-3">
          <div class="mb-0.5 text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('dashboard.widgets.add') }}
          </div>
          <button
            v-for="id in wl.addable" :key="id" type="button"
            class="flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm text-[var(--cn-text)] transition-colors hover:bg-[var(--cn-surface-hover)]"
            @click="wl.add(id)"
          >
            <AppIcon :name="WIDGET_REGISTRY[id].icon" :size="15" class="text-[var(--cn-text-muted)]" />
            {{ t(WIDGET_REGISTRY[id].nameKey) }}
            <AppIcon name="Plus" :size="14" class="ml-auto text-blue-500" />
          </button>
          <div v-if="!wl.addable.length" class="px-2 py-1 text-xs text-[var(--cn-text-muted)]">
            {{ t('dashboard.widgets.all_added') }}
          </div>
          <button
            type="button"
            class="mt-1 self-start px-2 text-xs text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]"
            @click="wl.reset()"
          >{{ t('dashboard.widgets.reset') }}</button>
        </div>
      </aside>

      <!-- Main column: tips → search → app grid. Tips sits at the top
           as the first thing the user sees — actionable onboarding
           moments earn top real estate. -->
      <main class="flex flex-col gap-4 md:gap-5 lg:gap-6">
        <TipsCarousel v-if="showTips" />
        <SearchBar />
        <!-- One unified grid: compose apps, standalone containers, and
             external compose stacks all render as first-class tiles here.
             The old separate "external containers" bucket is gone — those
             workloads are no longer second-class. -->
        <AppGrid />
      </main>
    </div>

    <!-- Footer wordmark: small branding line at the bottom of the
         viewport. CasaOS uses this slot for "Made with ❤ by IceWhale
         and YOU!" — we replace with the Leeway credit. -->
    <footer class="mt-8 flex items-center justify-center gap-2 text-xs text-[var(--cn-text-muted)]">
      <span class="font-semibold text-[var(--cn-text)]">CasaNova</span>
      <span>by</span>
      <span class="font-medium">Leeway Open Software</span>
    </footer>
  </div>
</template>
