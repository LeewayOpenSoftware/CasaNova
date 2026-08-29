<script setup lang="ts">
// DeleteConfirmDialog — confirmation prompt shown before a destructive
// delete. Accepts a list of paths (to handle multi-select delete) and
// shows a summary: "Delete N items?" with the first 5 names listed
// and "+ N more" for larger selections.
//
// Delete is intentionally NOT a PrimeVue confirmDialog() programmatic
// call because we want to keep control over the content (multi-item
// summary) and styling (red primary button).

import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useFilesStore } from '@/stores/files'

const props = defineProps<{
  visible: boolean
  paths: string[]
}>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const { t } = useI18n()
const files = useFilesStore()

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

// Resolve paths → entries for display. A path might resolve to
// undefined if the store was mutated between dialog open and render;
// we filter those out defensively.
const entries = computed(() =>
  props.paths
    .map((p) => {
      const parent = p.substring(0, p.lastIndexOf('/')) || '/'
      return files.getChildrenOf(parent).find((e) => e.path === p)
    })
    .filter((e): e is NonNullable<typeof e> => e !== undefined),
)

const previewNames = computed(() => entries.value.slice(0, 5).map((e) => e.name))
const extraCount = computed(() => Math.max(0, entries.value.length - 5))

function onConfirm() {
  files.deleteEntries(props.paths)
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
    :header="t('files.dialogs.delete.title', { count: entries.length })"
    :style="{ width: '480px' }"
    :dismissable-mask="true"
  >
    <div class="flex gap-4">
      <div class="flex h-10 w-10 flex-shrink-0 items-center justify-center rounded-full bg-red-500/15 text-red-500">
        <AppIcon name="AlertTriangle" :size="20" />
      </div>
      <div class="flex-1 text-sm text-[var(--cn-text)]">
        <p class="mb-2">{{ t('files.dialogs.delete.warning') }}</p>
        <ul class="list-inside list-disc space-y-0.5 text-xs text-[var(--cn-text-muted)]">
          <li v-for="n in previewNames" :key="n" class="truncate">{{ n }}</li>
          <li v-if="extraCount > 0" class="italic">
            {{ t('files.dialogs.delete.more', { count: extraCount }) }}
          </li>
        </ul>
      </div>
    </div>
    <template #footer>
      <Button :label="t('common.cancel')" severity="secondary" @click="onCancel" />
      <Button :label="t('common.delete')" severity="danger" @click="onConfirm" />
    </template>
  </Dialog>
</template>
