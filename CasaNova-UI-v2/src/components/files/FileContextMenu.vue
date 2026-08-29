<script setup lang="ts">
import AppIcon from '@/components/ui/AppIcon.vue'
// FileContextMenu — right-click menu that surfaces the common file
// operations (open, rename, download, delete). Uses PrimeVue's
// ContextMenu component which handles positioning, dismiss-on-click-
// outside, and keyboard navigation out of the box.
//
// The menu is parent-controlled via a ref: the parent calls .show(event)
// when a right-click lands on a file entry, passing the DOM event so
// the menu can position itself at the cursor. The parent also sets
// `targetPath` before showing so the item commands know which entry
// they're acting on.

import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import ContextMenu from 'primevue/contextmenu'

interface MenuItemDefinition {
  labelKey: string
  icon: string
  action:
    | 'open'
    | 'preview'
    | 'rename'
    | 'download'
    | 'delete'
    | 'info'
    | 'copy-path'
    | 'share'
    | 'share-link'
    | 'compress'
    | 'extract'
    | 'cut'
    | 'copy'
  danger?: boolean
  separatorBefore?: boolean
}

const props = defineProps<{
  targetIsDir: boolean
  // True when the target file looks like an archive (.zip/.tar.gz/…)
  // so the menu can offer Extract.
  targetIsArchive?: boolean
}>()

const emit = defineEmits<{
  (e: 'action', action: MenuItemDefinition['action']): void
}>()

const { t } = useI18n()
const menuRef = ref<InstanceType<typeof ContextMenu> | null>(null)

// Different items surface for folders vs files. Folders can be
// opened, shared (generates SMB paths), and have their path copied;
// files get preview + download on top of the common actions. Both
// get Cut/Copy/Paste-style actions (Copy path, Cut, Copy) so users
// have every clipboard affordance without opening a separate menu.
const items = computed<MenuItemDefinition[]>(() => {
  if (props.targetIsDir) {
    return [
      { labelKey: 'files.context.open', icon: 'FolderOpen', action: 'open' },
      { labelKey: 'files.context.share', icon: 'Share2', action: 'share' },
      { labelKey: 'files.context.share_link', icon: 'Link2', action: 'share-link' },
      { labelKey: 'files.context.compress', icon: 'Archive', action: 'compress' },
      { labelKey: 'files.context.download', icon: 'Download', action: 'download' },
      { labelKey: 'files.context.copy_path', icon: 'ClipboardCopy', action: 'copy-path' },
      { labelKey: 'files.context.rename', icon: 'Edit3', action: 'rename', separatorBefore: true },
      { labelKey: 'files.context.cut', icon: 'Scissors', action: 'cut' },
      { labelKey: 'files.context.copy', icon: 'Copy', action: 'copy' },
      { labelKey: 'files.context.info', icon: 'Info', action: 'info', separatorBefore: true },
      { labelKey: 'files.context.delete', icon: 'Trash2', action: 'delete', danger: true, separatorBefore: true },
    ]
  }
  const fileItems: MenuItemDefinition[] = [
    { labelKey: 'files.context.preview', icon: 'Eye', action: 'preview' },
    { labelKey: 'files.context.download', icon: 'Download', action: 'download' },
    { labelKey: 'files.context.share_link', icon: 'Link2', action: 'share-link' },
    { labelKey: 'files.context.compress', icon: 'Archive', action: 'compress' },
  ]
  if (props.targetIsArchive) {
    fileItems.push({ labelKey: 'files.context.extract', icon: 'PackageOpen', action: 'extract' })
  }
  fileItems.push(
    { labelKey: 'files.context.copy_path', icon: 'ClipboardCopy', action: 'copy-path' },
    { labelKey: 'files.context.rename', icon: 'Edit3', action: 'rename', separatorBefore: true },
    { labelKey: 'files.context.cut', icon: 'Scissors', action: 'cut' },
    { labelKey: 'files.context.copy', icon: 'Copy', action: 'copy' },
    { labelKey: 'files.context.info', icon: 'Info', action: 'info' },
    { labelKey: 'files.context.delete', icon: 'Trash2', action: 'delete', danger: true, separatorBefore: true },
  )
  return fileItems
})

// PrimeVue's ContextMenu expects a specific item shape with `label`
// and `command`. We map our internal definition to that shape on the
// fly so the intermediate MenuItemDefinition stays framework-agnostic.
const model = computed(() =>
  items.value.flatMap((item) => {
    const primeItem = {
      label: t(item.labelKey),
      icon: undefined, // we render our own icon (name string) via the item slot
      command: () => emit('action', item.action),
      _danger: item.danger,
      _iconName: item.icon,
    }
    return item.separatorBefore ? [{ separator: true }, primeItem] : [primeItem]
  }),
)

/**
 * Open the menu at the given DOM event's cursor position. Parent
 * component calls this from @contextmenu listeners on file tiles.
 */
function show(event: MouseEvent) {
  menuRef.value?.show(event)
}

defineExpose({ show })
</script>

<template>
  <ContextMenu ref="menuRef" :model="model">
    <template #item="{ item }">
      <div
        v-if="!item.separator"
        :class="[
          'flex cursor-pointer items-center gap-2 px-3 py-2 text-sm',
          item._danger ? 'text-red-500' : 'text-[var(--cn-text)]',
        ]"
      >
        <AppIcon :name="item._iconName" :size="15" />
        <span>{{ item.label }}</span>
      </div>
      <div
        v-else
        class="my-1 h-px bg-[var(--cn-border)]"
      />
    </template>
  </ContextMenu>
</template>
