<script setup lang="ts">
// About settings — version, credits, and links. The self-update engine used
// to live here; it now has its own dedicated Updates page (Settings →
// Updates), so About is purely informational.

import { onMounted, computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { useUpdatesStore } from '@/stores/updates'
import { systemApi } from '@/api'
import { safeCall } from '@/api/safeCall'

const { t } = useI18n()
const updates = useUpdatesStore()

const buildVersion = ref<string>('—')
const displayVersion = ref<string>(import.meta.env.VITE_DISPLAY_VERSION || '26.10')
const buildChannel = computed(() => updates.status.channel || 'dev')

onMounted(async () => {
  const result = await safeCall(() => systemApi.getCurrentVersion())
  if (result.data) buildVersion.value = String(result.data)
})

const credits: Array<{ label: string; url: string }> = [
  { label: 'CasaOS (IceWhaleTech)', url: 'https://github.com/IceWhaleTech/CasaOS' },
  { label: 'PrimeVue', url: 'https://primevue.org' },
  { label: 'Vue 3', url: 'https://vuejs.org' },
  { label: 'Vite', url: 'https://vitejs.dev' },
  { label: 'Tailwind CSS', url: 'https://tailwindcss.com' },
  { label: 'Lucide Icons', url: 'https://lucide.dev' },
]

function openExternal(url: string) {
  window.open(url, '_blank', 'noopener,noreferrer')
}
</script>

<template>
  <SettingsPage
    icon-src="/assets/brand/casanova.png"
    :title="t('settings.about.title')"
    :description="t('settings.about.description')"
  >
    <SettingsCard :title="t('settings.about.version')">
      <div class="flex items-center justify-between">
        <div>
          <div class="text-2xl font-semibold text-[var(--cn-text)]">CasaNova</div>
          <div class="mt-1 text-sm text-[var(--cn-text-muted)]">
            {{ t('settings.about.by') }}
            <span class="font-medium text-[var(--cn-text)]">Leeway Open Software</span>
          </div>
        </div>
        <div class="text-right">
          <div class="font-mono text-2xl font-bold text-[var(--cn-text)]">{{ displayVersion }}</div>
          <div class="mt-0.5 font-mono text-xs text-[var(--cn-text-muted)]">{{ buildVersion }}</div>
          <div
            class="mt-1 inline-flex items-center rounded-full px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide"
            :class="buildChannel === 'stable'
              ? 'bg-emerald-500/15 text-emerald-400'
              : buildChannel === 'beta'
                ? 'bg-amber-500/15 text-amber-400'
                : 'bg-blue-500/15 text-blue-500'"
          >
            {{ buildChannel }}
          </div>
        </div>
      </div>
      <p class="mt-3 border-t border-[var(--cn-border)] pt-3 text-xs text-[var(--cn-text-muted)]">
        {{ t('settings.about.update_moved') }}
        <router-link to="/app/settings/updates" class="text-blue-500 hover:underline">{{ t('settings.nav.updates') }}</router-link>.
      </p>
    </SettingsCard>

    <SettingsCard :title="t('settings.about.links')">
      <div class="flex flex-wrap gap-2">
        <button
          class="inline-flex items-center gap-2 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-3 py-2 text-sm text-[var(--cn-text)] transition-colors hover:bg-[var(--cn-border)]"
          @click="openExternal('https://github.com/LeewayOpenSoftware/CasaNova')"
        >
          <AppIcon name="Github" :size="16" />
          GitHub
          <AppIcon name="ExternalLink" :size="12" class="text-[var(--cn-text-muted)]" />
        </button>
      </div>
    </SettingsCard>

    <SettingsCard :title="t('settings.about.credits')" :description="t('settings.about.credits_desc')">
      <ul class="flex flex-wrap gap-2 text-sm">
        <li v-for="c in credits" :key="c.label">
          <button
            class="rounded-full border border-[var(--cn-border)] px-3 py-1 text-xs text-[var(--cn-text-muted)] transition-colors hover:border-blue-500 hover:text-blue-500"
            @click="openExternal(c.url)"
          >
            {{ c.label }}
          </button>
        </li>
      </ul>
      <p class="mt-4 flex items-center gap-2 text-xs text-[var(--cn-text-muted)]">
        {{ t('settings.about.made_with') }}
        <AppIcon name="Heart" :size="12" class="text-red-500" fill="currentColor" />
        {{ t('settings.about.made_with_suffix') }}
      </p>
    </SettingsCard>
  </SettingsPage>
</template>
