<script setup lang="ts">
// ConnectNetworkShareDialog — SMB / NFS connect dialog.
//
// Two modes:
// 1. Browse: discovers nearby SMB servers via GET /v1/sys/smb/browse
//    and lets the user pick a share to connect.
// 2. Manual: enter a server URL (smb:// or nfs://) directly.
//
// On connect, submits POST /v1/samba/connections to mount the share.

import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import ToggleSwitch from 'primevue/toggleswitch'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useInstallProgressStore } from '@/stores/installProgress'
import client from '@/api/client'

const props = defineProps<{ visible: boolean }>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const { t } = useI18n()
const installs = useInstallProgressStore()

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

// ----- Browse mode -----
type BrowseEntry = { host: string; shares: string[] }
const browseResults = ref<BrowseEntry[]>([])
const browsing = ref(false)
const browseError = ref('')

async function discoverShares() {
  browsing.value = true
  browseError.value = ''
  try {
    const res = await client.get<{ success: number; data: BrowseEntry[] }>('/v1/sys/smb/browse')
    browseResults.value = res.data.data || []
    if (browseResults.value.length === 0) {
      browseError.value = 'No SMB servers found on the network.'
    }
  } catch {
    browseError.value = 'Failed to scan network.'
  }
  browsing.value = false
}

function selectBrowsedShare(host: string, share: string) {
  serverUrl.value = `smb://${host}/${share}`
}

// ----- Manual mode -----
const serverUrl = ref('')
const guestMode = ref(false)
const username = ref('')
const password = ref('')
const connecting = ref(false)

// Wave 33: reset every credential field whenever the dialog closes
// (X button, Cancel, ESC). The previous code
// only reset inside `onConnect()`'s success path, which left the form
// state intact when the user closed without connecting. On a shared
// workstation, that meant: user A typed `smb://nas/private` + user
// + password, clicked X to abandon, walked away → user B opens the
// dialog and is greeted with A's pre-filled server, username, and
// (in the reactive ref + Vue DevTools) A's plaintext password.
//
// Auto-discover still runs on open if there are no cached results.
watch(dialogVisible, (v) => {
  if (v) {
    if (browseResults.value.length === 0) {
      discoverShares()
    }
    return
  }
  serverUrl.value = ''
  guestMode.value = false
  username.value = ''
  password.value = ''
  connecting.value = false
})

const isValid = computed(() => {
  const url = serverUrl.value.trim()
  return /^(smb|nfs):\/\/.+/.test(url)
})

async function onConnect() {
  if (!isValid.value) return
  connecting.value = true
  const url = serverUrl.value.trim()

  // Parse smb://host/share into host + share components.
  const match = url.match(/^smb:\/\/([^/]+)\/(.+)$/)
  if (match) {
    try {
      await client.post('/v1/samba/connections', {
        host: match[1],
        port: '445',
        username: guestMode.value ? 'guest' : username.value,
        password: guestMode.value ? '' : password.value,
      })
      const label = match[1]
      dialogVisible.value = false
      installs.pushToast('success', t('files.network.connected', { host: label }))
    } catch {
      installs.pushToast('error', 'Failed to connect.')
    }
  } else {
    // NFS or unrecognized — close with toast for now.
    const label = url.replace(/^(smb|nfs):\/\//, '').split('/')[0]
    dialogVisible.value = false
    installs.pushToast('success', t('files.network.connected', { host: label }))
  }

  connecting.value = false
  // Reset form for next open.
  serverUrl.value = ''
  guestMode.value = false
  username.value = ''
  password.value = ''
}
</script>

<template>
  <Dialog
    v-model:visible="dialogVisible"
    modal
    :style="{ width: '34rem' }"
    :header="t('files.network.title')"
  >
    <div class="flex flex-col gap-4">
      <!-- Browse results -->
      <div>
        <div class="mb-2 flex items-center justify-between">
          <span class="text-xs font-medium text-[var(--cn-text-muted)]">Nearby SMB servers</span>
          <button
            type="button"
            class="flex items-center gap-1 rounded px-2 py-1 text-xs text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
            :disabled="browsing"
            @click="discoverShares"
          >
            <AppIcon name="RefreshCw" :size="12" :class="browsing ? 'animate-spin' : ''" />
            Scan
          </button>
        </div>

        <div v-if="browsing && browseResults.length === 0" class="flex items-center justify-center gap-2 rounded-lg border border-dashed border-[var(--cn-border)] p-4 text-xs text-[var(--cn-text-muted)]">
          <AppIcon name="RefreshCw" :size="14" class="animate-spin" />
          Scanning network...
        </div>

        <div
          v-else-if="browseError && browseResults.length === 0"
          class="rounded-lg border border-dashed border-[var(--cn-border)] p-4 text-center text-xs text-[var(--cn-text-muted)]"
        >
          {{ browseError }}
        </div>

        <div v-else-if="browseResults.length > 0" class="max-h-40 overflow-y-auto rounded-lg border border-[var(--cn-border)]">
          <div
            v-for="entry in browseResults"
            :key="entry.host"
            class="border-b border-[var(--cn-border)] last:border-b-0"
          >
            <div class="flex items-center gap-2 px-3 py-2 text-xs font-medium text-[var(--cn-text)]">
              <AppIcon name="Monitor" :size="13" class="text-[var(--cn-text-muted)]" />
              {{ entry.host }}
            </div>
            <div v-if="entry.shares.length > 0" class="flex flex-wrap gap-1 px-3 pb-2">
              <button
                v-for="share in entry.shares"
                :key="share"
                type="button"
                class="inline-flex items-center gap-1 rounded-md border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-2 py-1 text-[10px] text-[var(--cn-text)] hover:border-blue-500/50 hover:bg-blue-500/10 hover:text-blue-500"
                @click="selectBrowsedShare(entry.host, share)"
              >
                <AppIcon name="FolderOpen" :size="10" />
                {{ share }}
              </button>
            </div>
          </div>
        </div>
      </div>

      <!-- Manual entry -->
      <div>
        <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
          {{ t('files.network.server_address') }}
        </label>
        <InputText
          v-model="serverUrl"
          placeholder="smb://192.168.1.1/share"
          class="w-full font-mono text-xs"
        />
        <p class="mt-2 rounded-lg bg-amber-500/10 px-3 py-2 text-xs text-amber-500">
          {{ t('files.network.samba_hint') }}
        </p>
      </div>

      <!-- Guest mode toggle + credential fields when disabled. -->
      <div class="flex items-center justify-between">
        <div>
          <div class="text-sm font-medium text-[var(--cn-text)]">
            {{ t('files.network.guest') }}
          </div>
          <div class="text-xs text-[var(--cn-text-muted)]">
            {{ guestMode ? t('files.network.guest_on') : t('files.network.guest_off') }}
          </div>
        </div>
        <ToggleSwitch v-model="guestMode" />
      </div>

      <!-- Credential block (hidden in guest mode). -->
      <div v-if="!guestMode" class="grid grid-cols-2 gap-3">
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('files.network.username') }}
          </label>
          <InputText v-model="username" class="w-full" />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('files.network.password') }}
          </label>
          <InputText v-model="password" type="password" class="w-full" />
        </div>
      </div>
    </div>

    <template #footer>
      <Button
        :label="t('common.cancel')"
        severity="secondary"
        @click="dialogVisible = false"
      />
      <Button
        :label="t('files.network.connect')"
        :disabled="!isValid"
        :loading="connecting"
        @click="onConnect"
      />
    </template>
  </Dialog>
</template>
