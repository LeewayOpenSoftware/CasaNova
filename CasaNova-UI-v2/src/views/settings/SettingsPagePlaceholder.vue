<script setup lang="ts">
// SettingsPagePlaceholder — temporary content for settings domains we
// haven't implemented yet. Every unbuilt page uses this component so
// users can navigate the full settings IA from day one while the real
// implementations ship incrementally.
//
// Each page passes: icon, title, description (all to SettingsPage),
// plus a list of planned features and the backend endpoints they'll
// consume. This gives devs cross-referencing the UI with the Go
// services a concrete "this page = that API" pairing while also
// communicating to users that the feature is on the roadmap.

import type { Component } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'

defineProps<{
  icon: Component
  title: string
  description: string
  /** Features this domain will expose in the UI once built. Plain strings. */
  features: string[]
  /** Backend endpoints the page will consume. Shown as code for devs. */
  endpoints?: string[]
  /** Phase 5 stage where this page is scheduled to be built. */
  stage: string
}>()

const { t } = useI18n()
</script>

<template>
  <SettingsPage :icon="icon" :title="title" :description="description">
    <!-- "What you'll find here" — sells the page's eventual value to
         users while making it clear nothing is wired yet. -->
    <SettingsCard :title="t('settings.placeholder.planned_features')">
      <ul class="flex flex-col gap-2">
        <li
          v-for="feature in features"
          :key="feature"
          class="flex items-start gap-2 text-sm text-[var(--cn-text-muted)]"
        >
          <AppIcon name="CheckCircle2" :size="16" class="mt-0.5 flex-shrink-0 text-blue-500" />
          <span>{{ feature }}</span>
        </li>
      </ul>
    </SettingsCard>

    <!-- Dev-facing pairing: the backend endpoints this page will call.
         Useful during the Phase 5 → backend wiring pass because it
         documents expected integrations inline without a separate doc. -->
    <SettingsCard
      v-if="endpoints && endpoints.length > 0"
      :title="t('settings.placeholder.backend_endpoints')"
      :description="t('settings.placeholder.backend_endpoints_desc')"
    >
      <ul class="flex flex-col gap-1 font-mono text-xs text-[var(--cn-text-muted)]">
        <li v-for="endpoint in endpoints" :key="endpoint">
          {{ endpoint }}
        </li>
      </ul>
    </SettingsCard>

    <!-- Stage badge — signals when to expect the real implementation. -->
    <SettingsCard :title="t('settings.placeholder.stage')">
      <div class="flex items-center gap-2 text-sm text-[var(--cn-text-muted)]">
        <AppIcon name="Zap" :size="16" class="text-amber-500" />
        <span>{{ t('settings.placeholder.planned_for', { stage }) }}</span>
      </div>
    </SettingsCard>
  </SettingsPage>
</template>
