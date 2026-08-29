<script setup lang="ts">
// NewFolderDialog — prompts for a folder name and creates it in the
// current directory via the store. Mounted by FileBrowserView and
// controlled via v-model. Auto-focuses the input on open and submits
// on Enter so keyboard-driven users can create-folder → name → Enter
// without touching the mouse.
//
// Collision check runs client-side against the store's current tree
// so the user gets immediate feedback if a folder with the same name
// already exists, rather than discovering it after submit.

import { ref, computed, watch, nextTick } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import InputText from 'primevue/inputtext'
import Button from 'primevue/button'
import { useFilesStore } from '@/stores/files'

const props = defineProps<{ visible: boolean }>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const { t } = useI18n()
const files = useFilesStore()

const name = ref('')
const inputRef = ref<InstanceType<typeof InputText> | null>(null)

// Proxy to allow two-way binding via v-model:visible without mutating
// the prop directly. Standard Vue 3 + PrimeVue pattern for dialogs.
const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

// Validation: name must be non-empty, contain no slashes, and not
// collide with an existing entry in the current directory.
const trimmed = computed(() => name.value.trim())
const hasCollision = computed(() =>
  files.getChildrenOf(files.currentPath).some((e) => e.name === trimmed.value),
)
const error = computed(() => {
  if (!trimmed.value) return ''
  if (trimmed.value.includes('/')) return t('files.dialogs.new_folder.error_slash')
  if (hasCollision.value) return t('files.dialogs.new_folder.error_exists')
  return ''
})
const canSubmit = computed(() => trimmed.value !== '' && error.value === '')

// Reset the input and focus it whenever the dialog opens. Focus is
// deferred to nextTick so PrimeVue's mount transition completes first.
watch(
  () => props.visible,
  async (open) => {
    if (open) {
      name.value = ''
      await nextTick()
      // PrimeVue wraps InputText in its own component; the underlying
      // DOM element lives on the $el property.
      const el = (inputRef.value as unknown as { $el: HTMLInputElement })?.$el
      el?.focus()
    }
  },
)

function onSubmit() {
  if (!canSubmit.value) return
  files.createFolder(trimmed.value)
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
    :header="t('files.dialogs.new_folder.title')"
    :style="{ width: '420px' }"
    :dismissable-mask="true"
  >
    <form class="flex flex-col gap-3" @submit.prevent="onSubmit">
      <label for="new-folder-name" class="text-sm font-medium text-[var(--cn-text-muted)]">
        {{ t('files.dialogs.new_folder.label') }}
      </label>
      <InputText
        id="new-folder-name"
        ref="inputRef"
        v-model="name"
        :placeholder="t('files.dialogs.new_folder.placeholder')"
        class="w-full"
        :invalid="error !== ''"
      />
      <p v-if="error" class="text-xs text-red-500">{{ error }}</p>
    </form>
    <template #footer>
      <Button :label="t('common.cancel')" severity="secondary" @click="onCancel" />
      <Button :label="t('common.create')" :disabled="!canSubmit" @click="onSubmit" />
    </template>
  </Dialog>
</template>
