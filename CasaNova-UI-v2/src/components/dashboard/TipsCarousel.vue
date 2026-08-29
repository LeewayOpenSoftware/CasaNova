<script setup lang="ts">
// TipsCarousel — horizontal strip of dismissible tip cards that surface
// actionable onboarding moments on the dashboard. CasaOS's equivalent
// showed cards like "Encontrado una nueva unidad" and "Sincroniza tus
// datos" — clickable prompts that guide the user into features they
// might not discover otherwise.
//
// This component is the first place the cn-glass variant shows up
// inside the dashboard (the login card is the only other user of it
// so far). Tips are a good fit for glass because:
//   1. They're ephemeral/promotional — they earn visual attention
//   2. They dismiss — the glassy affordance doesn't have to fight
//      with data density like a real widget
//   3. They layer visually over the rest of the dashboard, which is
//      exactly what a glass surface reads as
//
// Stage 5 integration: this component will consume a real tips source
// from the backend (a new /v1/sys/tips endpoint or a filter over the
// notifications feed with category=onboarding). For Stage 3 we use
// a local fixture so the carousel looks alive from day one.

import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useDashboardPrefsStore } from '@/stores/dashboardPrefs'

interface Tip {
  id: string
  icon: string
  titleKey: string
  bodyKey: string
  actionKey: string
  accentColor: string
}

const { t } = useI18n()
const dashboardPrefs = useDashboardPrefsStore()

// Fixture tips — each maps to a concrete CasaNova feature the user
// might want to discover. When we wire real data these become backend
// events emitted by the system (new disk detected, backup not set up,
// TLS not enabled, etc).
const allTips = ref<Tip[]>([
  {
    id: 'new-drive',
    icon: 'HardDrive',
    titleKey: 'tips.new_drive_title',
    bodyKey: 'tips.new_drive_body',
    actionKey: 'tips.new_drive_action',
    accentColor: 'rgb(59 130 246)', // blue-500
  },
  {
    id: 'setup-backup',
    icon: 'Cloud',
    titleKey: 'tips.backup_title',
    bodyKey: 'tips.backup_body',
    actionKey: 'tips.backup_action',
    accentColor: 'rgb(16 185 129)', // emerald-500
  },
  {
    id: 'enable-https',
    icon: 'ShieldCheck',
    titleKey: 'tips.https_title',
    bodyKey: 'tips.https_body',
    actionKey: 'tips.https_action',
    accentColor: 'rgb(168 85 247)', // purple-500
  },
])

// Visible tips — the full fixture filtered by the persisted dismissed
// set. Tips the user has closed stay closed across reloads thanks to
// the dashboardPrefs Pinia store, and come back via the "Restore tips"
// button in General settings.
const tips = computed(() =>
  allTips.value.filter((tip) => !dashboardPrefs.isDismissed(tip.id)),
)

// Index of the currently-visible tip. The carousel rotates one at a
// time; prev/next arrows wrap around. An auto-advance could go here
// later but we skip it for Stage 3 — rotating tips feel manipulative.
const activeIndex = ref(0)
const activeTip = computed(() => tips.value[activeIndex.value])

// Keep the index valid when the filtered list shrinks (user dismisses
// the currently-visible tip) or grows (user restores via settings).
watch(tips, (list) => {
  if (activeIndex.value >= list.length) {
    activeIndex.value = Math.max(0, list.length - 1)
  }
})

function next() {
  if (tips.value.length === 0) return
  activeIndex.value = (activeIndex.value + 1) % tips.value.length
}

function prev() {
  if (tips.value.length === 0) return
  activeIndex.value = (activeIndex.value - 1 + tips.value.length) % tips.value.length
}

function dismiss(id: string) {
  dashboardPrefs.dismissTip(id)
}
</script>

<template>
  <!-- Container sized for the main column; collapses gracefully to
       nothing when every tip is dismissed. -->
  <div v-if="activeTip" class="cn-glass relative p-5">
    <!-- Accent bar on the left edge of the card — picks up the tip's
         theme color so the user associates each tip with its domain
         without needing to read the body. -->
    <div
      class="absolute left-0 top-0 h-full w-1 rounded-l-[20px]"
      :style="{ backgroundColor: activeTip.accentColor }"
    />

    <div class="flex items-start gap-4">
      <!-- Icon well: tinted with the tip's accent color at low alpha
           so it reads as part of the card surface, not a stuck-on element. -->
      <div
        class="flex h-12 w-12 flex-shrink-0 items-center justify-center rounded-xl"
        :style="{
          backgroundColor: `${activeTip.accentColor}1f`,
          color: activeTip.accentColor,
        }"
      >
        <AppIcon :name="activeTip.icon" :size="24" />
      </div>

      <div class="min-w-0 flex-1">
        <h4 class="text-sm font-semibold text-[var(--cn-text)]">{{ t(activeTip.titleKey) }}</h4>
        <p class="mt-1 text-sm text-[var(--cn-text-muted)]">{{ t(activeTip.bodyKey) }}</p>
        <button
          class="mt-3 inline-flex items-center rounded-lg px-3 py-1.5 text-xs font-medium text-white transition-colors hover:brightness-110"
          :style="{ backgroundColor: activeTip.accentColor }"
        >
          {{ t(activeTip.actionKey) }}
        </button>
      </div>

      <!-- Top-right controls: prev / next / dismiss. Prev and next are
           hidden when there's only one tip left. -->
      <div class="flex items-center gap-1">
        <template v-if="tips.length > 1">
          <button
            class="cn-icon-btn h-7 w-7 text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]"
            :aria-label="t('tips.previous')"
            @click="prev"
          >
            <AppIcon name="ChevronLeft" :size="14" />
          </button>
          <button
            class="cn-icon-btn h-7 w-7 text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]"
            :aria-label="t('tips.next')"
            @click="next"
          >
            <AppIcon name="ChevronRight" :size="14" />
          </button>
        </template>
        <button
          class="cn-icon-btn h-7 w-7 text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]"
          :aria-label="t('tips.dismiss')"
          @click="dismiss(activeTip.id)"
        >
          <AppIcon name="X" :size="14" />
        </button>
      </div>
    </div>

    <!-- Pagination dots — tiny visual hint that more tips exist.
         Only renders when there are 2+ tips. -->
    <div v-if="tips.length > 1" class="mt-4 flex items-center justify-center gap-1.5">
      <button
        v-for="(tip, i) in tips"
        :key="tip.id"
        :class="[
          'h-1.5 rounded-full transition-all',
          i === activeIndex
            ? 'w-6 bg-white/70'
            : 'w-1.5 bg-white/25 hover:bg-white/40',
        ]"
        :aria-label="`Go to tip ${i + 1}`"
        @click="activeIndex = i"
      />
    </div>
  </div>
</template>
