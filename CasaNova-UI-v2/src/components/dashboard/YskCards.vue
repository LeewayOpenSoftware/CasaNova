<script setup lang="ts">
// YskCards — renders the live YSK (You Should Know) card list on the
// dashboard's aside column. Each card is dismissible (DELETE
// /v2/message_bus/ysk/{id}), and the backend seeds three onboarding
// cards on first boot (Data Station / File Management / Remote Access).
//
// Rendering: the YSKCardContent shape lets a card carry exactly one of
// three body variants — progress bar, icon-with-text, or list. We
// fan out on which field is populated; the backend never emits more
// than one variant per card. Footer actions are rendered as PrimeVue
// buttons; clicking dispatches the messageBus payload to the backend's
// action API (best-effort POST — failure surfaces only in dev console
// because the user already saw the click happen).

import { onMounted, onBeforeUnmount, computed } from 'vue'
import { storeToRefs } from 'pinia'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { useYskStore } from '@/stores/ysk'
import Button from 'primevue/button'
import AppIcon from '@/components/ui/AppIcon.vue'
import type { YSKCardFooterAction } from '@/api/ysk'
import client from '@/api/client'

const { t } = useI18n()
const router = useRouter()

const ysk = useYskStore()
const { visibleCards, loading } = storeToRefs(ysk)

// Stable empty state — when the store hasn't fetched yet OR the user
// dismissed every card, render nothing rather than an empty box. The
// aside column already has other widgets, so a placeholder here would
// just be visual noise.
const hasCards = computed(() => visibleCards.value.length > 0)

onMounted(() => {
  ysk.startPolling()
})

onBeforeUnmount(() => {
  // Polling lives at the store level — stopping when this component
  // unmounts (route change away from the dashboard) keeps the
  // background HTTP traffic correlated with what the user is actually
  // looking at.
  ysk.stopPolling()
})

async function dispatchAction(action: YSKCardFooterAction): Promise<void> {
  const mb = action.messageBus
  if (!mb || !mb.sourceID || !mb.name) return

  // Client-side intercept: cards seeded by CasaNova's own onboarding
  // publisher use `casanova-ui:navigate` to ask the UI to push a
  // route. Handle it locally rather than round-tripping through the
  // bus, both because there's no backend listener for it and because
  // navigation feels instant when it doesn't go via the network.
  if (mb.sourceID === 'casanova-ui' && mb.name === 'casanova-ui:navigate') {
    const path = mb.properties?.path
    if (path) router.push(path)
    return
  }

  // POST /v2/message_bus/action/{sourceID}/{name} mirrors the
  // backend's action publish endpoint. Best-effort — we don't surface
  // errors because the user already saw the click happen; we just log.
  try {
    await client.post(
      `/v2/message_bus/action/${encodeURIComponent(mb.sourceID)}/${encodeURIComponent(mb.name)}`,
      mb.properties ?? {},
    )
  } catch (err) {
    // eslint-disable-next-line no-console
    console.warn('[ysk] action dispatch failed', err)
  }
}

async function onDismiss(id: string): Promise<void> {
  try {
    await ysk.dismiss(id)
  } catch (err) {
    // Surface via console only — the store's optimistic-rollback path
    // re-adds the card so the user sees it didn't dismiss without us
    // needing a toast.
    // eslint-disable-next-line no-console
    console.warn('[ysk] dismiss failed', err)
  }
}

// Tailwind-friendly button class for each PrimeVue style key. We
// don't pass the raw style string to <Button> because the YSK schema
// allows arbitrary strings (`primary`, `secondary`, `text`, anything
// the backend invents next) and PrimeVue silently ignores unknown
// `severity` props. Mapping here keeps unknown values rendering as a
// neutral secondary button rather than disappearing.
function severityFor(style: string): 'primary' | 'secondary' | 'success' | 'info' | 'warning' | 'danger' | 'contrast' | undefined {
  switch (style) {
    case 'primary': return 'primary'
    case 'secondary': return 'secondary'
    case 'text': return undefined
    case 'success': return 'success'
    case 'warning': return 'warning'
    case 'danger': return 'danger'
    default: return 'secondary'
  }
}
</script>

<template>
  <section
    v-if="hasCards"
    :aria-label="t('ysk.section_title')"
    class="flex flex-col gap-3"
  >
    <h2 class="text-xs font-semibold uppercase tracking-wide text-slate-500">
      {{ t('ysk.section_title') }}
    </h2>

    <article
      v-for="card in visibleCards"
      :key="card.id"
      class="cn-widget group relative p-4"
    >
      <!-- Dismiss button — corner X. Stays muted until hover so the
           cards don't feel littered with controls. -->
      <button
        type="button"
        :aria-label="t('ysk.dismiss')"
        class="absolute right-2 top-2 rounded p-1 text-slate-400 opacity-0 transition-opacity hover:bg-slate-100 hover:text-slate-700 group-hover:opacity-100 dark:hover:bg-slate-700 dark:hover:text-slate-200"
        @click="onDismiss(card.id)"
      >
        <AppIcon name="X" :size="14" />
      </button>

      <!-- Title row — icon + text. The icon field is free-form on the
           backend (URL or lucide name); AppIcon handles both via its
           own resolution chain. -->
      <header class="mb-3 flex items-center gap-2 pr-6">
        <AppIcon v-if="card.content.titleIcon" :name="card.content.titleIcon" :size="16" />
        <h3 class="text-sm font-semibold text-slate-900 dark:text-slate-100">
          {{ card.content.titleText }}
        </h3>
      </header>

      <!-- Body — exactly one variant per card per the YSK schema. -->
      <div class="mb-3 text-xs text-slate-600 dark:text-slate-300">
        <!-- Variant 1: progress bar. -->
        <div v-if="card.content.bodyProgress" class="flex flex-col gap-1">
          <div class="flex items-center justify-between">
            <span>{{ card.content.bodyProgress.label }}</span>
            <span class="tabular-nums">{{ card.content.bodyProgress.progress }}%</span>
          </div>
          <div class="h-1.5 overflow-hidden rounded-full bg-slate-200 dark:bg-slate-700">
            <div
              class="h-full rounded-full bg-blue-500 transition-all"
              :style="{ width: `${Math.min(100, Math.max(0, card.content.bodyProgress.progress))}%` }"
            />
          </div>
        </div>

        <!-- Variant 2: icon + description. -->
        <div v-else-if="card.content.bodyIconWithText" class="flex items-center gap-2">
          <AppIcon
            v-if="card.content.bodyIconWithText.icon"
            :name="card.content.bodyIconWithText.icon"
            :size="20"
            class="shrink-0"
          />
          <p class="leading-relaxed">{{ card.content.bodyIconWithText.description }}</p>
        </div>

        <!-- Variant 3: list of items. -->
        <ul
          v-else-if="card.content.bodyList && card.content.bodyList.length > 0"
          class="flex flex-col gap-1.5"
        >
          <li
            v-for="(item, i) in card.content.bodyList"
            :key="i"
            class="flex items-center gap-2"
          >
            <AppIcon v-if="item.icon" :name="item.icon" :size="14" class="shrink-0" />
            <span class="flex-1">{{ item.description }}</span>
            <span class="text-slate-500 tabular-nums">{{ item.rightText }}</span>
          </li>
        </ul>
      </div>

      <!-- Footer actions — split left/right. -->
      <footer
        v-if="card.content.footerActions && card.content.footerActions.length > 0"
        class="flex flex-wrap items-center justify-between gap-2"
      >
        <div class="flex flex-wrap gap-2">
          <Button
            v-for="(a, i) in card.content.footerActions.filter((x) => x.side === 'left')"
            :key="`l${i}`"
            :label="a.text"
            :severity="severityFor(a.style)"
            :text="a.style === 'text'"
            size="small"
            @click="dispatchAction(a)"
          />
        </div>
        <div class="flex flex-wrap gap-2">
          <Button
            v-for="(a, i) in card.content.footerActions.filter((x) => x.side === 'right')"
            :key="`r${i}`"
            :label="a.text"
            :severity="severityFor(a.style)"
            :text="a.style === 'text'"
            size="small"
            @click="dispatchAction(a)"
          />
        </div>
      </footer>
    </article>

    <!-- Loading indicator only on the initial fetch — subsequent
         polls render silently to keep the dashboard from flickering. -->
    <div
      v-if="loading && !hasCards"
      class="text-xs text-slate-400"
    >
      {{ t('ysk.loading') }}
    </div>
  </section>
</template>
