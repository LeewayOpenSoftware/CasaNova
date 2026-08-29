<script setup lang="ts">
import AppIcon from '@/components/ui/AppIcon.vue'
// MigrationSettings — read-only inspection page for what was carried
// over from a prior CasaOS install. Fresh CasaNova installs see an
// all-zero report; CasaOS upgraders see a populated one. The page is
// safe to leave in nav permanently — it just shows nothing
// interesting to people who never had CasaOS.

import { onMounted, ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Button from 'primevue/button'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { getMigrationReport, acknowledgeMigration, type MigrationReport } from '@/api/migration'
import { safeCall } from '@/api/safeCall'

const { t } = useI18n()

const report = ref<MigrationReport | null>(null)
const loading = ref(true)
const ackInProgress = ref(false)
const errorMsg = ref<string | null>(null)

// A migration is "interesting" only when at least one signal points
// at a prior CasaOS install. Fresh CasaNova users see nothing on this
// page beyond a single-line "no migration in progress" reassurance.
const hasLegacyState = computed(() => {
  if (!report.value) return false
  return (
    report.value.legacy_config_dir ||
    report.value.configs_migrated.length > 0 ||
    report.value.legacy_compose_apps.length > 0 ||
    report.value.legacy_containers > 0 ||
    report.value.legacy_units.length > 0
  )
})

// Pretty-print byte sizes for the config table. CasaOS configs are
// almost always <10KB so we never need MB scaling.
function fmtBytes(n: number): string {
  if (n < 0) return '—'
  if (n < 1024) return `${n} B`
  return `${(n / 1024).toFixed(1)} KB`
}

async function reload(): Promise<void> {
  loading.value = true
  errorMsg.value = null
  const result = await safeCall(() => getMigrationReport())
  loading.value = false
  if (result.error) {
    errorMsg.value = result.error
    return
  }
  if (result.data) {
    report.value = result.data
  }
}

async function onAcknowledge(): Promise<void> {
  ackInProgress.value = true
  const result = await safeCall(() => acknowledgeMigration())
  ackInProgress.value = false
  if (result.error) {
    errorMsg.value = result.error
    return
  }
  // Refresh so the "acknowledged" badge updates without a full page
  // reload — keeps the page reactive to state changes from this tab.
  await reload()
}

onMounted(() => {
  reload()
})
</script>

<template>
  <div class="flex flex-col gap-6">
    <header class="flex flex-col gap-1">
      <h1 class="text-xl font-semibold">{{ t('settings.migration.title') }}</h1>
      <p class="text-sm text-slate-500 dark:text-slate-400">
        {{ t('settings.migration.subtitle') }}
      </p>
    </header>

    <div v-if="errorMsg" class="rounded border border-red-300 bg-red-50 p-3 text-sm text-red-700 dark:border-red-900 dark:bg-red-950 dark:text-red-300">
      <AppIcon name="AlertCircle" :size="16" class="mr-2 inline" />
      {{ errorMsg }}
    </div>

    <!-- No legacy state — fresh CasaNova install. Single calm card so
         the page never reads as "broken" or "empty". -->
    <SettingsCard v-if="report && !hasLegacyState && !loading" :title="t('settings.migration.fresh_title')">
      <p class="flex items-start gap-2 text-sm text-slate-600 dark:text-slate-300">
        <AppIcon name="CheckCircle2" :size="18" class="mt-0.5 shrink-0 text-emerald-500" />
        <span>{{ t('settings.migration.fresh_body') }}</span>
      </p>
    </SettingsCard>

    <!-- Legacy state detected — show the report. -->
    <template v-else-if="report && hasLegacyState">
      <!-- Status banner -->
      <SettingsCard :title="t('settings.migration.status_title')">
        <div class="flex flex-col gap-2 text-sm">
          <div class="flex items-center gap-2">
            <AppIcon name="CheckCircle2" v-if="report.acknowledged" :size="16" class="text-emerald-500" />
            <AppIcon name="AlertCircle" v-else :size="16" class="text-amber-500" />
            <span>{{ report.acknowledged ? t('settings.migration.status_acked') : t('settings.migration.status_pending') }}</span>
          </div>
          <div class="text-xs text-slate-500">
            {{ t('settings.migration.generated_at', { time: report.generated_at }) }}
          </div>
          <div class="flex gap-2 pt-2">
            <Button
              v-if="!report.acknowledged"
              :label="ackInProgress ? t('common.saving') : t('settings.migration.acknowledge')"
              :disabled="ackInProgress"
              severity="primary"
              size="small"
              @click="onAcknowledge"
            />
            <Button :label="t('common.refresh')" severity="secondary" size="small" @click="reload" />
          </div>
        </div>
      </SettingsCard>

      <!-- Configs migrated to /etc/casanova/ -->
      <SettingsCard
        v-if="report.configs_migrated.length > 0"
        :title="t('settings.migration.configs_title')"
        :description="t('settings.migration.configs_desc')"
      >
        <ul class="flex flex-col divide-y divide-slate-200 text-sm dark:divide-slate-700">
          <li
            v-for="cfg in report.configs_migrated"
            :key="cfg.path"
            class="flex items-center gap-3 py-2"
          >
            <AppIcon name="Settings" :size="16" class="shrink-0 text-slate-400" />
            <code class="flex-1 truncate text-xs">{{ cfg.path }}</code>
            <span class="tabular-nums text-xs text-slate-500">{{ fmtBytes(cfg.size) }}</span>
          </li>
        </ul>
      </SettingsCard>

      <!-- Compose apps under /var/lib/casaos/apps/ -->
      <SettingsCard
        v-if="report.legacy_compose_apps.length > 0"
        :title="t('settings.migration.compose_title')"
        :description="t('settings.migration.compose_desc', { dir: report.legacy_compose_apps_dir })"
      >
        <ul class="flex flex-wrap gap-2">
          <li
            v-for="name in report.legacy_compose_apps"
            :key="name"
            class="inline-flex items-center gap-1.5 rounded-full bg-slate-100 px-3 py-1 text-xs dark:bg-slate-800"
          >
            <AppIcon name="FolderOpen" :size="12" />
            {{ name }}
          </li>
        </ul>
      </SettingsCard>

      <!-- Containers carrying casaos=casaos label -->
      <SettingsCard
        v-if="report.legacy_containers > 0"
        :title="t('settings.migration.containers_title')"
      >
        <p class="flex items-center gap-2 text-sm">
          <AppIcon name="Container" :size="16" class="text-slate-400" />
          {{ t('settings.migration.containers_count', { n: report.legacy_containers }) }}
        </p>
      </SettingsCard>

      <!-- Legacy systemd units -->
      <SettingsCard
        v-if="report.legacy_units.length > 0"
        :title="t('settings.migration.units_title')"
        :description="t('settings.migration.units_desc')"
      >
        <ul class="flex flex-col divide-y divide-slate-200 text-sm dark:divide-slate-700">
          <li
            v-for="unit in report.legacy_units"
            :key="unit.name"
            class="flex items-center gap-3 py-2"
          >
            <AppIcon name="Server" :size="16" class="shrink-0 text-slate-400" />
            <code class="flex-1 truncate text-xs">{{ unit.name }}</code>
            <span
              class="rounded px-2 py-0.5 text-xs"
              :class="unit.active ? 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-200' : 'bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300'"
            >
              {{ unit.active ? t('settings.migration.unit_active') : t('settings.migration.unit_inactive') }}
            </span>
            <span
              class="rounded px-2 py-0.5 text-xs"
              :class="unit.enabled ? 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-200' : 'bg-slate-100 text-slate-600 dark:bg-slate-800 dark:text-slate-300'"
            >
              {{ unit.enabled ? t('settings.migration.unit_enabled') : t('settings.migration.unit_disabled') }}
            </span>
          </li>
        </ul>
      </SettingsCard>
    </template>

    <!-- Loading skeleton -->
    <SettingsCard v-else-if="loading" :title="t('common.loading')">
      <p class="text-sm text-slate-500">{{ t('settings.migration.scanning') }}</p>
    </SettingsCard>
  </div>
</template>
