<script setup lang="ts">
// ConnectCloudProviderDialog — generic cloud connector sheet. One
// component handles Google Drive, Dropbox, and OneDrive; the parent
// picks the provider via the `provider` prop and the dialog renders
// the matching branding + OAuth explainer.
//
// When the backend wiring lands, "Connect" opens a popup window to
// the provider's OAuth authorize endpoint, receives the callback
// token, and mounts the cloud storage as a virtual filesystem under
// the sidebar's Storage section. For the scaffold we just push a
// toast and close the dialog so the flow is visible.

import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useInstallProgressStore } from '@/stores/installProgress'

export type CloudProvider = 'gdrive' | 'dropbox' | 'onedrive'

const props = defineProps<{
  visible: boolean
  provider: CloudProvider | null
}>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const { t } = useI18n()
const installs = useInstallProgressStore()

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

// Provider metadata — display name + brand color for the icon tile.
// The real OAuth URLs land in the backend wiring pass.
const providerMeta = {
  gdrive: { name: 'Google Drive', color: '#4285F4' },
  dropbox: { name: 'Dropbox', color: '#0061FF' },
  onedrive: { name: 'OneDrive', color: '#0078D4' },
} as const

const meta = computed(() => (props.provider ? providerMeta[props.provider] : null))

function onConnect() {
  if (!meta.value) return
  dialogVisible.value = false
  installs.pushToast('info', t('files.cloud.connecting', { provider: meta.value.name }))
}
</script>

<template>
  <Dialog
    v-if="meta"
    v-model:visible="dialogVisible"
    modal
    :style="{ width: '28rem' }"
    :dismissable-mask="true"
    :header="t('files.cloud.connect_title', { provider: meta.name })"
  >
    <div class="flex flex-col items-center gap-4 py-2 text-center">
      <div
        class="flex h-16 w-16 items-center justify-center rounded-2xl text-white shadow-md"
        :style="{ backgroundColor: meta.color }"
      >
        <AppIcon name="Cloud" :size="32" />
      </div>
      <div>
        <div class="text-base font-semibold text-[var(--cn-text)]">{{ meta.name }}</div>
        <p class="mt-1 max-w-xs text-xs text-[var(--cn-text-muted)]">
          {{ t('files.cloud.description', { provider: meta.name }) }}
        </p>
      </div>
    </div>

    <!-- Permissions explainer — a little trust-building line so users
         know what they're about to authorize. -->
    <ul class="mt-2 flex flex-col gap-2 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-3 text-xs">
      <li class="flex items-start gap-2">
        <AppIcon name="ShieldCheck" :size="13" class="mt-0.5 text-emerald-500" />
        <span class="text-[var(--cn-text)]">{{ t('files.cloud.perm_read') }}</span>
      </li>
      <li class="flex items-start gap-2">
        <AppIcon name="ShieldCheck" :size="13" class="mt-0.5 text-emerald-500" />
        <span class="text-[var(--cn-text)]">{{ t('files.cloud.perm_mount') }}</span>
      </li>
      <li class="flex items-start gap-2">
        <AppIcon name="Lock" :size="13" class="mt-0.5 text-[var(--cn-text-muted)]" />
        <span class="text-[var(--cn-text-muted)]">{{ t('files.cloud.perm_local') }}</span>
      </li>
    </ul>

    <template #footer>
      <Button
        :label="t('common.cancel')"
        severity="secondary"
        @click="dialogVisible = false"
      />
      <Button :label="t('files.cloud.sign_in')" @click="onConnect" />
    </template>
  </Dialog>
</template>
