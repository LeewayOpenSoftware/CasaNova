<script setup lang="ts">
// General settings — the one real settings page in the first landing.
// Shows: language picker, dark/light mode toggle, a read-only hostname
// display (placeholder until the backend endpoint ships), and a
// timezone display. Everything that can be set from the client
// (language, theme) is functional today; backend-sourced values
// render as read-only rows with a "coming soon" badge next to them.

import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import { setPref } from '@/composables/syncedPrefs'
import Select from 'primevue/select'
import InputText from 'primevue/inputtext'
import Button from 'primevue/button'
import ToggleSwitch from 'primevue/toggleswitch'
import { systemApi } from '@/api'
import { safeCall } from '@/api/safeCall'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { useDevMode } from '@/composables/useDevMode'
import { useUserPrefs, SEARCH_ENGINES, type SearchEngine } from '@/composables/useUserPrefs'
import { useDashboardPrefsStore } from '@/stores/dashboardPrefs'

const { devMode, advancedMode } = useDevMode()
const { searchEngine, showTips } = useUserPrefs()
const dashboardPrefs = useDashboardPrefsStore()

// ----- System identity (hostname + timezone) -----
// Both are backend-sourced. Hostname is edited via hostnamectl; the
// timezone list comes from /usr/share/zoneinfo so the picker is the
// real IANA list rather than a hand-curated subset.
const hostname = ref('')
const hostnameInput = ref('')
const hostnameSaving = ref(false)
const hostnameError = ref('')

const timezone = ref('')
const timezoneList = ref<string[]>([])
const timezoneSaving = ref(false)
const timezoneError = ref('')

async function loadIdentity() {
  const hn = await safeCall(() => systemApi.getHostname())
  if (hn.data !== undefined) {
    hostname.value = hn.data
    hostnameInput.value = hn.data
  }
  const tz = await safeCall(() => systemApi.getTimezone())
  if (tz.data) {
    timezone.value = tz.data.timezone
    timezoneList.value = tz.data.list
  }
}
async function saveHostname() {
  if (!hostnameInput.value || hostnameInput.value === hostname.value) return
  hostnameError.value = ''
  hostnameSaving.value = true
  const res = await safeCall(() => systemApi.setHostname(hostnameInput.value))
  hostnameSaving.value = false
  if (res.error) {
    hostnameError.value = res.error
    return
  }
  hostname.value = hostnameInput.value
}
async function saveTimezone(tz: string) {
  if (!tz || tz === timezone.value) return
  timezoneError.value = ''
  timezoneSaving.value = true
  const res = await safeCall(() => systemApi.setTimezone(tz))
  timezoneSaving.value = false
  if (res.error) {
    timezoneError.value = res.error
    return
  }
  timezone.value = tz
}
onMounted(loadIdentity)

// Search engine options — labels are human-readable; values are the
// short codes the composable uses to look up the URL template.
const searchEngineOptions = computed(() =>
  (Object.keys(SEARCH_ENGINES) as SearchEngine[]).map((code) => ({
    code,
    label: SEARCH_ENGINES[code].label,
  })),
)

const { t, locale, availableLocales } = useI18n()

// Language picker — reads the available locales registered in the i18n
// instance so the list stays in sync as we add translations. Display
// labels come from a small lookup table below.
const localeOptions = computed(() =>
  availableLocales.map((code) => ({
    code,
    label: localeDisplay(code),
  })),
)

function localeDisplay(code: string): string {
  switch (code) {
    case 'en_US':
      return 'English'
    case 'es_ES':
      return 'Español'
    case 'pt_BR':
      return 'Português (Brasil)'
    default:
      return code
  }
}

function onLocaleChange(option: { code: string; label: string } | string) {
  // PrimeVue Select can emit either the object or just the value depending
  // on the bindings, so accept both shapes defensively.
  const code = typeof option === 'string' ? option : option.code
  locale.value = code as typeof locale.value
  // v0.1: route through syncedPrefs so language choice follows the
  // account on next login from another browser.
  setPref('casanova.lang', code)
}
</script>

<template>
  <SettingsPage
    icon="Settings"
    :title="t('settings.general.title')"
    :description="t('settings.general.description')"
  >
    <!-- Language picker. vue-i18n's locale ref is reactive, so the
         moment we change it the entire UI re-renders in the new
         language. Stored to localStorage so the choice survives a
         page reload. -->
    <SettingsCard :title="t('settings.general.language')" :description="t('settings.general.language_desc')">
      <template #actions>
        <div class="flex items-center gap-2.5">
          <AppIcon name="Globe" :size="16" class="text-[var(--cn-text-muted)]" />
          <Select
            :model-value="locale"
            :options="localeOptions"
            option-label="label"
            option-value="code"
            class="w-48 sm:w-60"
            @update:model-value="onLocaleChange"
          />
        </div>
      </template>
    </SettingsCard>

    <!-- Search engine — feeds the dashboard search bar. Adding a new
         engine is a one-line change in useUserPrefs's SEARCH_ENGINES
         map; the picker here is auto-populated. -->
    <SettingsCard
      :title="t('settings.general.search_engine')"
      :description="t('settings.general.search_engine_desc')"
    >
      <template #actions>
        <div class="flex items-center gap-2.5">
          <AppIcon name="Search" :size="16" class="text-[var(--cn-text-muted)]" />
          <Select
            v-model="searchEngine"
            :options="searchEngineOptions"
            option-label="label"
            option-value="code"
            class="w-48 sm:w-60"
          />
        </div>
      </template>
    </SettingsCard>

    <!-- Dashboard tips — opt-out for users who've seen all the hints.
         Also hosts the "Restore dismissed tips" action so users who
         closed individual cards can bring them back without having to
         clear localStorage. -->
    <SettingsCard
      :title="t('settings.general.dashboard_tips')"
      :description="t('settings.general.dashboard_tips_desc')"
    >
      <template #actions>
        <ToggleSwitch v-model="showTips" />
      </template>

      <!-- Restore row — only shown when at least one tip has been
           dismissed, so the setting stays clean by default. -->
      <div
        v-if="dashboardPrefs.dismissedList.length > 0"
        class="mt-4 flex items-center justify-between border-t border-[var(--cn-border)] pt-4"
      >
        <div class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.general.dismissed_tips_count', { n: dashboardPrefs.dismissedList.length }, dashboardPrefs.dismissedList.length) }}
        </div>
        <button
          type="button"
          class="text-xs text-blue-500 hover:underline"
          @click="dashboardPrefs.restoreAllTips()"
        >
          {{ t('settings.general.restore_tips') }}
        </button>
      </div>
    </SettingsCard>

    <!-- Developer mode — gates experimental/not-yet-ready features
         across the dashboard. Off by default; power users flip it on
         to see the in-progress surfaces without shipping half-baked
         UI to everyone else. -->
    <SettingsCard
      :title="t('settings.general.dev_mode')"
      :description="t('settings.general.dev_mode_desc')"
    >
      <template #actions>
        <ToggleSwitch v-model="devMode" />
      </template>

      <!-- Advanced developer mode — second toggle that unlocks
           destructive lifecycle actions (zpool create / destroy /
           replace, future btrfs balance, firmware flashes). Visually
           subordinate to the dev-mode toggle but kept in the same
           card so the relationship is clear. The accent-red warning
           bar makes it impossible to flip casually. -->
      <div class="mt-3 flex items-center justify-between border-t border-[var(--cn-border)] pt-3">
        <div class="flex items-center gap-3">
          <div class="flex h-10 w-10 items-center justify-center rounded-lg bg-red-500/15 text-red-500">
            <AppIcon name="ShieldAlert" :size="18" />
          </div>
          <div>
            <div class="text-sm font-medium text-[var(--cn-text)]">
              {{ t('settings.general.advanced_mode_label') }}
            </div>
            <div class="text-xs text-[var(--cn-text-muted)]">
              {{ advancedMode
                ? t('settings.general.advanced_mode_on')
                : t('settings.general.advanced_mode_off') }}
            </div>
          </div>
        </div>
        <ToggleSwitch v-model="advancedMode" />
      </div>
    </SettingsCard>

    <!-- Hostname + timezone — editable, backend-persisted. -->
    <SettingsCard :title="t('settings.general.system_info')">
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div>
          <label class="mb-1 block text-xs text-[var(--cn-text-muted)]">
            {{ t('settings.general.hostname') }}
          </label>
          <div class="flex items-center gap-2">
            <InputText
              v-model="hostnameInput"
              class="flex-1 font-mono text-sm"
              :disabled="hostnameSaving"
              @keyup.enter="saveHostname"
            />
            <Button
              size="small"
              severity="secondary"
              :disabled="hostnameSaving || hostnameInput === hostname || !hostnameInput"
              :loading="hostnameSaving"
              @click="saveHostname"
            >
              {{ t('common.save') }}
            </Button>
          </div>
          <p v-if="hostnameError" class="mt-1 text-xs text-red-500">{{ hostnameError }}</p>
        </div>
        <div>
          <label class="mb-1 block text-xs text-[var(--cn-text-muted)]">
            {{ t('settings.general.timezone') }}
          </label>
          <Select
            :model-value="timezone"
            :options="timezoneList"
            filter
            :disabled="timezoneSaving"
            :placeholder="timezone || 'UTC'"
            class="w-full"
            @update:model-value="saveTimezone"
          />
          <p v-if="timezoneError" class="mt-1 text-xs text-red-500">{{ timezoneError }}</p>
        </div>
      </div>
    </SettingsCard>
  </SettingsPage>
</template>
