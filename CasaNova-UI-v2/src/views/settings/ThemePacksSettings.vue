<script setup lang="ts">
import AppIcon from '@/components/ui/AppIcon.vue'
// ThemePacksSettings — upload, list, delete, and activate uploaded
// theme packs. A theme pack is a .zip with a theme.json + tokens.css
// + optional assets/, served from the backend at /v1/sys/themes/<id>/.
// Switching to a pack is the same as picking a built-in variant in
// General → Appearance — we set themeVariant = `custom:<id>` on the
// theme store, which then auto-injects the pack's tokens.css.

import { ref, onMounted, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Button from 'primevue/button'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { listThemePacks, uploadThemePack, deleteThemePack, type ThemePackInfo } from '@/api/themePacks'
import { safeCall } from '@/api/safeCall'
import { useThemeStore } from '@/stores/theme'

const { t } = useI18n()
const theme = useThemeStore()

const packs = ref<ThemePackInfo[]>([])
const loading = ref(true)
const uploading = ref(false)
const errorMsg = ref<string | null>(null)
const successMsg = ref<string | null>(null)

const fileInput = ref<HTMLInputElement | null>(null)

const activePackId = computed<string | null>(() => {
  const v = theme.prefs.themeVariant
  return v.startsWith('custom:') ? v.slice('custom:'.length) : null
})

async function reload(): Promise<void> {
  loading.value = true
  errorMsg.value = null
  const result = await safeCall(() => listThemePacks())
  loading.value = false
  if (result.error) {
    errorMsg.value = result.error
    return
  }
  packs.value = result.data ?? []
}

function openPicker(): void {
  fileInput.value?.click()
}

async function onFileChange(e: Event): Promise<void> {
  const target = e.target as HTMLInputElement
  const file = target.files?.[0]
  if (!file) return
  // Reset the input so picking the same filename twice still fires.
  target.value = ''

  // Client-side sanity — the backend caps at 16 MiB, so we reject
  // anything over that here to give the user a faster failure.
  if (file.size > 16 * 1024 * 1024) {
    errorMsg.value = t('settings.theme_packs.too_large')
    return
  }
  if (!file.name.toLowerCase().endsWith('.zip')) {
    errorMsg.value = t('settings.theme_packs.not_zip')
    return
  }

  uploading.value = true
  errorMsg.value = null
  successMsg.value = null
  const result = await safeCall(() => uploadThemePack(file))
  uploading.value = false
  if (result.error) {
    errorMsg.value = result.error
    return
  }
  if (result.data) {
    successMsg.value = t('settings.theme_packs.uploaded', { name: result.data.name })
  }
  await reload()
}

async function onDelete(pack: ThemePackInfo): Promise<void> {
  if (!confirm(t('settings.theme_packs.confirm_delete', { name: pack.name }))) return
  const result = await safeCall(() => deleteThemePack(pack.id))
  if (result.error) {
    errorMsg.value = result.error
    return
  }
  // If the just-deleted pack was active, fall back to the default
  // variant so we don't leave a broken <link> trying to load CSS
  // from a now-deleted backend path.
  if (activePackId.value === pack.id) {
    theme.setThemeVariant('default')
  }
  await reload()
}

function onActivate(pack: ThemePackInfo): void {
  theme.setThemeVariant(`custom:${pack.id}`)
  successMsg.value = t('settings.theme_packs.activated', { name: pack.name })
}

onMounted(() => {
  reload()
})
</script>

<template>
  <SettingsPage
    icon="Palette"
    :title="t('settings.theme_packs.title')"
    :description="t('settings.theme_packs.subtitle')"
  >
    <!-- Upload -->
    <SettingsCard
      :title="t('settings.theme_packs.upload_title')"
      :description="t('settings.theme_packs.upload_desc')"
    >
      <div class="flex flex-col gap-3">
        <input
          ref="fileInput"
          type="file"
          accept=".zip,application/zip,application/x-zip-compressed"
          class="hidden"
          @change="onFileChange"
        />
        <Button
          :label="uploading ? t('settings.theme_packs.uploading') : t('settings.theme_packs.upload_button')"
          severity="primary"
          :disabled="uploading"
          @click="openPicker"
        >
          <template #icon><AppIcon name="Upload" :size="14" /></template>
        </Button>
        <p class="text-xs text-slate-500">
          {{ t('settings.theme_packs.upload_hint') }}
        </p>
      </div>
    </SettingsCard>

    <!-- Status banners -->
    <div
      v-if="errorMsg"
      class="flex items-start gap-2 rounded border border-red-300 bg-red-50 p-3 text-sm text-red-700 dark:border-red-900 dark:bg-red-950 dark:text-red-300"
    >
      <AppIcon name="AlertCircle" :size="16" class="mt-0.5" />
      <span>{{ errorMsg }}</span>
    </div>
    <div
      v-if="successMsg"
      class="flex items-start gap-2 rounded border border-emerald-300 bg-emerald-50 p-3 text-sm text-emerald-800 dark:border-emerald-900 dark:bg-emerald-950 dark:text-emerald-200"
    >
      <AppIcon name="CheckCircle2" :size="16" class="mt-0.5" />
      <span>{{ successMsg }}</span>
    </div>

    <!-- Installed packs list -->
    <SettingsCard :title="t('settings.theme_packs.installed_title')">
      <div v-if="loading" class="text-sm text-slate-500">{{ t('common.loading') }}</div>
      <div
        v-else-if="packs.length === 0"
        class="text-sm text-slate-500"
      >
        {{ t('settings.theme_packs.empty') }}
      </div>
      <ul v-else class="flex flex-col divide-y divide-slate-200 dark:divide-slate-700">
        <li
          v-for="pack in packs"
          :key="pack.id"
          class="flex flex-col gap-2 py-3 sm:flex-row sm:items-center sm:justify-between"
        >
          <div class="flex flex-col">
            <div class="flex items-center gap-2 text-sm font-semibold">
              {{ pack.name }}
              <span
                v-if="activePackId === pack.id"
                class="rounded-full bg-emerald-100 px-2 py-0.5 text-xs text-emerald-700 dark:bg-emerald-900 dark:text-emerald-200"
              >
                {{ t('settings.theme_packs.active') }}
              </span>
            </div>
            <div class="text-xs text-slate-500">
              <span v-if="pack.author">{{ pack.author }} · </span>
              <span v-if="pack.version">v{{ pack.version }} · </span>
              <code>{{ pack.id }}</code>
            </div>
            <p v-if="pack.description" class="mt-1 text-xs text-slate-600 dark:text-slate-300">
              {{ pack.description }}
            </p>
          </div>
          <div class="flex gap-2">
            <Button
              v-if="activePackId !== pack.id"
              :label="t('settings.theme_packs.activate')"
              severity="primary"
              size="small"
              @click="onActivate(pack)"
            />
            <Button
              severity="danger"
              size="small"
              :title="t('settings.theme_packs.delete')"
              @click="onDelete(pack)"
            >
              <template #icon><AppIcon name="Trash2" :size="14" /></template>
            </Button>
          </div>
        </li>
      </ul>
    </SettingsCard>
  </SettingsPage>
</template>
