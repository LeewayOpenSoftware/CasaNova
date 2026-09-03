<script setup lang="ts">
import { skipWhenHidden } from '@/composables/skipWhenHidden'
import AppIcon from '@/components/ui/AppIcon.vue'
// RemoteAccessSettings — inventory page for the three remote-access
// connectors CasaNova knows how to detect: Tailscale, Cloudflare
// Tunnel, ZeroTier. We don't install any of them inline — Tailscale
// and ZeroTier are native systemd services with their own installer
// scripts, and Cloudflared ships as an app-catalog container. This
// page surfaces status and points at the right install path.

import { onMounted, onBeforeUnmount, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { useLibraryStore } from '@/stores/library'
import Button from 'primevue/button'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { getRemoteAccessStatus, type ConnectorStatus } from '@/api/remoteAccess'
import { safeCall } from '@/api/safeCall'

const { t } = useI18n()
const router = useRouter()
const library = useLibraryStore()

const connectors = ref<ConnectorStatus[]>([])
const loading = ref(true)
const lastError = ref<string | null>(null)

// Lucide icon map per connector. We embed it here rather than in the
// API client because the icons are pure presentation — the backend
// only cares about IDs and human names.
const iconFor: Record<ConnectorStatus['id'], string> = {
  tailscale: 'Globe',
  cloudflared: 'Cloud',
  zerotier: 'Radio',
}

// Single 30s poll — connector status doesn't change on the second-scale,
// and the page is unlikely to be the user's primary view, so we keep
// the cadence relaxed.
let pollTimer: ReturnType<typeof setInterval> | null = null
const POLL_MS = 30_000

async function reload(): Promise<void> {
  loading.value = true
  lastError.value = null
  const result = await safeCall(() => getRemoteAccessStatus())
  loading.value = false
  if (result.error) {
    lastError.value = result.error
    return
  }
  if (result.data) {
    connectors.value = result.data
  }
}

function openInstall(c: ConnectorStatus): void {
  if (!c.install_url) return
  // Three URL shapes supported by the connector status backend:
  //   library:<term>  → open the in-app library panel pre-searched
  //   /…             → push an internal Vue route
  //   http(s)://…    → external upstream installer
  if (c.install_url.startsWith('library:')) {
    const term = c.install_url.slice('library:'.length)
    library.setSearch(term)
    library.openPanel()
    return
  }
  if (c.install_url.startsWith('/')) {
    router.push(c.install_url)
  } else {
    window.open(c.install_url, '_blank', 'noopener,noreferrer')
  }
}

onMounted(() => {
  reload()
  pollTimer = setInterval(skipWhenHidden(reload), POLL_MS)
})

onBeforeUnmount(() => {
  if (pollTimer) clearInterval(pollTimer)
})
</script>

<template>
  <SettingsPage
    icon="Globe"
    :title="t('settings.remote_access.title')"
    :description="t('settings.remote_access.subtitle')"
  >
    <SettingsCard :title="t('settings.remote_access.intro_title')">
      <p class="text-sm text-slate-600 dark:text-slate-300">
        {{ t('settings.remote_access.intro_body') }}
      </p>
    </SettingsCard>

    <div v-if="lastError" class="rounded border border-red-300 bg-red-50 p-3 text-sm text-red-700 dark:border-red-900 dark:bg-red-950 dark:text-red-300">
      <AppIcon name="AlertCircle" :size="16" class="mr-2 inline" />
      {{ lastError }}
    </div>

    <div class="flex justify-end">
      <Button
        :label="t('common.refresh')"
        severity="secondary"
        size="small"
        text
        @click="reload"
      >
        <template #icon>
          <AppIcon name="RefreshCw" :size="14" />
        </template>
      </Button>
    </div>

    <SettingsCard
      v-for="c in connectors"
      :key="c.id"
      :title="c.name"
    >
      <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div class="flex items-start gap-3">
          <component :is="iconFor[c.id]" :size="28" class="shrink-0 text-slate-500" />
          <div class="flex flex-col gap-1">
            <!-- Status pill. Three states, kept simple so the user
                 can scan the page in two seconds. -->
            <div class="flex items-center gap-2">
              <AppIcon name="CheckCircle2"
                v-if="c.installed && c.running"
                :size="16"
                class="text-emerald-500"
              />
              <AppIcon name="AlertCircle"
                v-else-if="c.installed && !c.running"
                :size="16"
                class="text-amber-500"
              />
              <AppIcon name="XCircle" v-else :size="16" class="text-slate-400" />
              <span class="text-sm font-semibold">
                {{ c.installed
                    ? c.running
                      ? t('settings.remote_access.status_running')
                      : t('settings.remote_access.status_installed_not_running')
                    : t('settings.remote_access.status_not_installed') }}
              </span>
            </div>
            <p v-if="c.detail" class="text-xs text-slate-500">
              {{ c.detail }}
            </p>
          </div>
        </div>
        <div class="flex gap-2">
          <Button
            v-if="!c.installed && c.install_url"
            :label="t('settings.remote_access.install')"
            severity="primary"
            size="small"
            @click="openInstall(c)"
          >
            <template #icon>
              <AppIcon name="ExternalLink" :size="14" />
            </template>
          </Button>
        </div>
      </div>
    </SettingsCard>

    <div v-if="loading && connectors.length === 0" class="text-sm text-slate-500">
      {{ t('common.loading') }}
    </div>
  </SettingsPage>
</template>
