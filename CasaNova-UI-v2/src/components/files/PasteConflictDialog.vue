<script setup lang="ts">
/**
 * PasteConflictDialog — shown when the user pastes files whose basenames
 * already exist in the destination directory.
 *
 * Choices:
 *   - Overwrite all  → default backend behaviour (style omitted), the
 *                       destination entries are replaced.
 *   - Skip existing  → style="skip" on the batch request; collisions
 *                       are left alone, non-colliding files still copy.
 *   - Cancel         → close without dispatching.
 *
 * "Rename automatically" is intentionally not offered yet — the backend
 * `FileOperate.Style` has no "rename" mode, and adding " (N)" suffixes
 * client-side would require a per-file re-issue of the operation. Will
 * land alongside a backend `style: "rename"` flag in a follow-up.
 */
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import AppIcon from '@/components/ui/AppIcon.vue'

const props = defineProps<{
  visible: boolean
  collisions: string[]
  destination: string
}>()
const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
  (e: 'choose', v: 'overwrite' | 'skip' | 'cancel'): void
}>()

const { t } = useI18n()

function dispatch(choice: 'overwrite' | 'skip' | 'cancel') {
  emit('choose', choice)
  emit('update:visible', false)
}
</script>

<template>
  <Dialog
    :visible="props.visible"
    modal
    :style="{ width: '30rem' }"
    :header="t('files.paste_conflict.title')"
    :dismissable-mask="false"
    :closable="false"
  >
    <div class="flex flex-col gap-3">
      <div class="flex items-start gap-3">
        <div class="rounded-lg bg-amber-500/15 p-2 text-amber-500">
          <AppIcon name="AlertTriangle" :size="18" />
        </div>
        <div class="flex-1 text-sm text-[var(--cn-text)]">
          {{ t('files.paste_conflict.summary', { n: props.collisions.length, dest: props.destination }) }}
        </div>
      </div>
      <ul
        class="max-h-40 overflow-auto rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-2 text-xs font-mono"
      >
        <li
          v-for="name in props.collisions.slice(0, 50)"
          :key="name"
          class="truncate text-[var(--cn-text)]"
        >
          {{ name }}
        </li>
        <li
          v-if="props.collisions.length > 50"
          class="mt-1 text-[var(--cn-text-muted)]"
        >
          … +{{ props.collisions.length - 50 }} {{ t('files.paste_conflict.more') }}
        </li>
      </ul>
    </div>

    <template #footer>
      <Button
        :label="t('common.cancel')"
        severity="secondary"
        @click="dispatch('cancel')"
      />
      <Button
        :label="t('files.paste_conflict.skip')"
        severity="secondary"
        @click="dispatch('skip')"
      />
      <Button
        :label="t('files.paste_conflict.overwrite')"
        severity="danger"
        @click="dispatch('overwrite')"
      />
    </template>
  </Dialog>
</template>
