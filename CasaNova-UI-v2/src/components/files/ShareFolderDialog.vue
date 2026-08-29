<script setup lang="ts">
// ShareFolderDialog — confirms a folder has been published over SMB
// and surfaces the paths Windows + macOS clients need to connect.
//
// Host resolution: uses `window.location.hostname` by default so the
// example path reflects however the user reached CasaNova. Real
// backend sets up the actual Samba share when the user clicks Share
// on a folder for the first time; this dialog just displays the
// resulting URIs.

import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import AppIcon from '@/components/ui/AppIcon.vue'

const props = defineProps<{
  visible: boolean
  folderPath: string | null
}>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const { t } = useI18n()

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

// Build the share paths for Windows and macOS. Takes the last segment
// of the folder path as the share name (matches what most SMB
// configurations default to). The host is whatever the browser is
// currently pointing at — real backend plugs in the configured hostname.
const shareName = computed(() => {
  if (!props.folderPath) return ''
  const parts = props.folderPath.split('/').filter(Boolean)
  return parts[parts.length - 1] ?? 'share'
})
const host = computed(() => window.location.hostname || 'casanova.local')
const windowsPath = computed(() => `\\\\${host.value}\\${shareName.value}`)
const macPath = computed(() => `smb://${host.value}/${shareName.value}`)

const copied = ref<'win' | 'mac' | null>(null)
async function copyToClipboard(text: string, which: 'win' | 'mac') {
  try {
    await navigator.clipboard.writeText(text)
    copied.value = which
    setTimeout(() => (copied.value = null), 1500)
  } catch {
    /* clipboard blocked */
  }
}
</script>

<template>
  <Dialog
    v-model:visible="dialogVisible"
    modal
    :style="{ width: '30rem' }"
    :header="t('files.share.title')"
    :dismissable-mask="true"
  >
    <p class="mb-4 text-sm text-[var(--cn-text-muted)]">
      {{ t('files.share.description') }}
    </p>

    <div class="flex flex-col gap-3">
      <!-- Windows path -->
      <div>
        <div class="mb-1 text-xs font-medium text-[var(--cn-text)]">
          {{ t('files.share.windows') }}
        </div>
        <div class="flex items-center gap-2">
          <code class="flex-1 truncate rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-3 py-2 font-mono text-xs text-[var(--cn-text)]">
            {{ windowsPath }}
          </code>
          <button
            type="button"
            class="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg border border-[var(--cn-border)] text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
            :aria-label="t('files.share.copy')"
            @click="copyToClipboard(windowsPath, 'win')"
          >
            <AppIcon v-if="copied === 'win'" name="Check" :size="14" class="text-emerald-500" />
            <AppIcon v-else name="Copy" :size="14" />
          </button>
        </div>
      </div>

      <!-- Mac path -->
      <div>
        <div class="mb-1 text-xs font-medium text-[var(--cn-text)]">
          {{ t('files.share.mac') }}
        </div>
        <div class="flex items-center gap-2">
          <code class="flex-1 truncate rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-3 py-2 font-mono text-xs text-[var(--cn-text)]">
            {{ macPath }}
          </code>
          <button
            type="button"
            class="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg border border-[var(--cn-border)] text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
            :aria-label="t('files.share.copy')"
            @click="copyToClipboard(macPath, 'mac')"
          >
            <AppIcon v-if="copied === 'mac'" name="Check" :size="14" class="text-emerald-500" />
            <AppIcon v-else name="Copy" :size="14" />
          </button>
        </div>
      </div>
    </div>

    <p class="mt-4 text-xs text-[var(--cn-text-muted)]">
      {{ t('files.share.credentials_hint') }}
    </p>

    <template #footer>
      <Button :label="t('files.share.done')" @click="dialogVisible = false" />
    </template>
  </Dialog>
</template>
