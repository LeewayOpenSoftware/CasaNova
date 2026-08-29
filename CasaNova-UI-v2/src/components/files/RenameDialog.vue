<script setup lang="ts">
// RenameDialog — renames the currently-targeted entry (single select
// only). Triggered from the context menu or the F2 keyboard shortcut.
//
// Pre-fills the input with the entry's current name on open, and
// pre-selects just the base name (everything before the last dot) so
// users editing a file's name don't have to hand-deselect the
// extension. Folders auto-select the whole name.

import { ref, computed, watch, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import InputText from 'primevue/inputtext'
import Button from 'primevue/button'
import { useFilesStore } from '@/stores/files'

const props = defineProps<{
  visible: boolean
  targetPath: string | null
}>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const { t } = useI18n()
const files = useFilesStore()

const name = ref('')
const inputRef = ref<InstanceType<typeof InputText> | null>(null)

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

// Look up the current entry so we know whether it's a file or folder
// (for the pre-selection logic) and show the correct header text.
const target = computed(() => {
  if (!props.targetPath) return null
  const parent = props.targetPath.substring(0, props.targetPath.lastIndexOf('/')) || '/'
  return files.getChildrenOf(parent).find((e) => e.path === props.targetPath) ?? null
})

const trimmed = computed(() => name.value.trim())
const unchanged = computed(() => target.value && trimmed.value === target.value.name)
const hasCollision = computed(() => {
  if (!target.value || unchanged.value) return false
  const parent = target.value.path.substring(0, target.value.path.lastIndexOf('/')) || '/'
  return files.getChildrenOf(parent).some((e) => e.name === trimmed.value)
})
const error = computed(() => {
  if (!trimmed.value) return ''
  if (trimmed.value.includes('/')) return t('files.dialogs.new_folder.error_slash')
  if (hasCollision.value) return t('files.dialogs.new_folder.error_exists')
  return ''
})
const canSubmit = computed(() => trimmed.value !== '' && error.value === '' && !unchanged.value)

watch(
  () => props.visible,
  async (open) => {
    if (open && target.value) {
      name.value = target.value.name
      await nextTick()
      const el = (inputRef.value as unknown as { $el: HTMLInputElement })?.$el
      if (el) {
        el.focus()
        // Pre-select the base name so hitting a key immediately
        // replaces the name without deleting the extension.
        if (!target.value.isDir) {
          const lastDot = target.value.name.lastIndexOf('.')
          if (lastDot > 0) {
            el.setSelectionRange(0, lastDot)
          } else {
            el.select()
          }
        } else {
          el.select()
        }
      }
    }
  },
)

function onSubmit() {
  if (!canSubmit.value || !props.targetPath) return
  files.renameEntry(props.targetPath, trimmed.value)
  dialogVisible.value = false
}

function onCancel() {
  dialogVisible.value = false
}
</script>

<template>
  <Dialog
    v-model:visible="dialogVisible"
    modal
    :header="t('files.dialogs.rename.title')"
    :style="{ width: '420px' }"
    :dismissable-mask="true"
  >
    <form class="flex flex-col gap-3" @submit.prevent="onSubmit">
      <label for="rename-input" class="text-sm font-medium text-[var(--cn-text-muted)]">
        {{ t('files.dialogs.rename.label') }}
      </label>
      <InputText
        id="rename-input"
        ref="inputRef"
        v-model="name"
        class="w-full"
        :invalid="error !== ''"
      />
      <p v-if="error" class="text-xs text-red-500">{{ error }}</p>
    </form>
    <template #footer>
      <Button :label="t('common.cancel')" severity="secondary" @click="onCancel" />
      <Button :label="t('files.dialogs.rename.submit')" :disabled="!canSubmit" @click="onSubmit" />
    </template>
  </Dialog>
</template>
