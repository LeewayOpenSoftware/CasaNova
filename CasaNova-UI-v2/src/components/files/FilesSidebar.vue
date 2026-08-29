<script setup lang="ts">
// FilesSidebar — the left navigation rail for the file manager.
//
// Two sections:
//   1. Favorites — curated shortcuts into commonly-used folders
//      (Documents, Downloads, Gallery, Media, Backups). Each is a
//      one-click jump via the files store's cd() method.
//   2. Storage — mounted drives and cloud targets. Populated from
//      GET /v2/local_storage/storage, with a sensible default of a
//      single "DATA" entry if the API returns empty.
//
// The active entry is highlighted with the same blue accent used by
// SettingsNav so the two sidebars feel like the same system.

import { computed, ref, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import Menu from 'primevue/menu'
import type { MenuItem } from 'primevue/menuitem'
import { useFilesStore } from '@/stores/files'
import { storageApi } from '@/api'
import { safeCall } from '@/api/safeCall'
import QuickDiskPickerDialog from './QuickDiskPickerDialog.vue'
import ConnectCloudProviderDialog, { type CloudProvider } from './ConnectCloudProviderDialog.vue'
import ConnectNetworkShareDialog from './ConnectNetworkShareDialog.vue'

// Dialog visibility state. Each entry in the + menu opens a distinct
// dialog rather than routing away to Settings → Storage, matching the
// "slim in-place popup" UX feedback.
const quickDiskVisible = ref(false)
const cloudDialogVisible = ref(false)
const cloudDialogProvider = ref<CloudProvider | null>(null)
const networkDialogVisible = ref(false)

function openCloud(provider: CloudProvider) {
  cloudDialogProvider.value = provider
  cloudDialogVisible.value = true
}

const { t } = useI18n()
const files = useFilesStore()

// Sidebar favorites — well-known shortcuts into the filesystem.
// These are UI-only shortcuts, not served by the backend.
type FavoriteIconKey = 'documents' | 'downloads' | 'gallery' | 'media' | 'backup'
interface FavoriteEntry {
  path: string
  labelKey: string
  iconKey: FavoriteIconKey
}
const favorites: FavoriteEntry[] = [
  { path: '/DATA/Documents', labelKey: 'files.favorites.documents', iconKey: 'documents' },
  { path: '/DATA/Downloads', labelKey: 'files.favorites.downloads', iconKey: 'downloads' },
  { path: '/DATA/Gallery', labelKey: 'files.favorites.gallery', iconKey: 'gallery' },
  { path: '/DATA/Media', labelKey: 'files.favorites.media', iconKey: 'media' },
  { path: '/DATA/Backups', labelKey: 'files.favorites.backup', iconKey: 'backup' },
]

const iconByKey: Record<FavoriteIconKey, string> = {
  documents: 'FileText',
  downloads: 'Download',
  gallery: 'Image',
  media: 'Film',
  backup: 'Archive',
}

// An entry is "active" when the current browse path starts with its
// own path — this makes nested navigation visible in the sidebar too
// (browsing into /DATA/Documents/Taxes 2025 keeps Documents highlighted).
function isActive(path: string): boolean {
  return files.currentPath === path || files.currentPath.startsWith(path + '/')
}

function navigate(path: string) {
  files.cd(path)
}

// Storage section — fetches real mounted drives from the backend.
// Falls back to a single DATA entry if the API isn't available yet.
const fetchedRoots = ref<{ id: string; label: string; path: string }[]>([])
const storageRoots = computed(() =>
  fetchedRoots.value.length > 0
    ? fetchedRoots.value
    : [{ id: 'data', label: 'DATA', path: '/DATA' }],
)

onMounted(async () => {
  const result = await safeCall(() => storageApi.listStorageRoots())
  if (result.data && result.data.length > 0) {
    fetchedRoots.value = result.data.map((r) => ({
      id: r.id,
      label: r.label,
      path: r.path,
    }))
  }
})

// "+" menu — offered options match the CasaOS Files add-storage menu:
// a local folder, three cloud connectors, and a generic SMB/NFS
// network share. Every action is a no-op stub for now — the backend
// needed for cloud OAuth + mount points lands later.
const addStorageMenu = ref<InstanceType<typeof Menu> | null>(null)
function openAddStorage(event: MouseEvent) {
  addStorageMenu.value?.toggle(event)
}
const addStorageItems = computed<MenuItem[]>(() => [
  {
    label: t('files.storage_menu.local'),
    icon: 'cn-local',
    command: () => (quickDiskVisible.value = true),
  },
  { separator: true },
  {
    label: t('files.storage_menu.gdrive'),
    icon: 'cn-gdrive',
    command: () => openCloud('gdrive'),
  },
  {
    label: t('files.storage_menu.dropbox'),
    icon: 'cn-dropbox',
    command: () => openCloud('dropbox'),
  },
  {
    label: t('files.storage_menu.onedrive'),
    icon: 'cn-onedrive',
    command: () => openCloud('onedrive'),
  },
  { separator: true },
  {
    label: t('files.storage_menu.network'),
    icon: 'cn-network',
    command: () => (networkDialogVisible.value = true),
  },
])
</script>

<template>
  <nav class="cn-widget flex flex-col gap-5 p-3">
    <!-- Brand-ish header for the sidebar. Small, clickable (returns
         to root), sets the visual anchor for the whole manager. -->
    <button
      class="flex items-center gap-3 rounded-lg px-3 py-2 text-left transition-colors hover:bg-[var(--cn-surface-hover)]"
      @click="navigate('/')"
    >
      <div class="flex h-8 w-8 items-center justify-center rounded-lg bg-blue-500/15 text-blue-500">
        <AppIcon name="HardDrive" :size="18" />
      </div>
      <div class="min-w-0 flex-1">
        <div class="text-sm font-semibold text-[var(--cn-text)]">{{ t('files.title') }}</div>
      </div>
    </button>

    <!-- Favorites section. Small header, then one entry per fixture. -->
    <div class="flex flex-col gap-1">
      <div class="px-3 pb-1 text-[10px] font-semibold uppercase tracking-wider text-[var(--cn-text-muted)]">
        {{ t('files.sidebar.favorites') }}
      </div>
      <button
        v-for="fav in favorites"
        :key="fav.path"
        :class="[
          'flex items-center gap-3 rounded-lg px-3 py-2 text-sm transition-colors',
          isActive(fav.path)
            ? 'cn-nav-active'
            : 'text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]',
        ]"
        @click="navigate(fav.path)"
      >
        <AppIcon :name="iconByKey[fav.iconKey]" :size="16" />
        <span class="truncate">{{ t(fav.labelKey) }}</span>
      </button>
    </div>

    <!-- Storage section. Same header style as Favorites. Each root is
         a drive; clicking navigates to its top-level directory. A "+"
         button at the end is the call to action for mounting a new
         drive (navigates to Settings → Storage when that page lands). -->
    <div class="flex flex-col gap-1">
      <div class="flex items-center justify-between px-3 pb-1">
        <span class="text-[10px] font-semibold uppercase tracking-wider text-[var(--cn-text-muted)]">
          {{ t('files.sidebar.storage') }}
        </span>
        <button
          class="cn-icon-btn h-5 w-5"
          :aria-label="t('files.sidebar.add_storage')"
          @click="openAddStorage"
        >
          <AppIcon name="Plus" :size="12" />
        </button>
        <Menu ref="addStorageMenu" :model="addStorageItems" :popup="true">
          <template #item="{ item, props: iprops }">
            <a
              v-bind="iprops.action"
              class="flex cursor-pointer items-center gap-2 px-3 py-2 text-sm text-[var(--cn-text)]"
            >
              <AppIcon v-if="item.icon === 'cn-local'" name="HardDrive" :size="14" />
              <AppIcon v-else-if="item.icon === 'cn-gdrive'" name="Cloud" :size="14" class="text-blue-500" />
              <AppIcon v-else-if="item.icon === 'cn-dropbox'" name="Cloud" :size="14" class="text-blue-400" />
              <AppIcon v-else-if="item.icon === 'cn-onedrive'" name="Cloud" :size="14" class="text-sky-500" />
              <AppIcon v-else-if="item.icon === 'cn-network'" name="Network" :size="14" />
              <span>{{ item.label }}</span>
            </a>
          </template>
        </Menu>
      </div>
      <button
        v-for="root in storageRoots"
        :key="root.id"
        :class="[
          'flex items-center gap-3 rounded-lg px-3 py-2 text-sm transition-colors',
          isActive(root.path)
            ? 'cn-nav-active'
            : 'text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]',
        ]"
        @click="navigate(root.path)"
      >
        <AppIcon name="HardDrive" :size="16" />
        <span class="truncate">{{ root.label }}</span>
      </button>
    </div>

    <!-- Bottom section — shared folders view. Separated by a thin
         border so it reads as a distinct destination rather than a
         fifth favorite. -->
    <div class="mt-auto border-t border-[var(--cn-border)] pt-4">
      <button
        :class="[
          'flex w-full items-center gap-3 rounded-lg px-3 py-2 text-sm transition-colors',
          $route.path.endsWith('/files/shared')
            ? 'cn-nav-active'
            : 'text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]',
        ]"
        @click="$router.push('/app/files/shared')"
      >
        <AppIcon name="Share2" :size="16" />
        <span class="truncate">{{ t('files.sidebar.shared') }}</span>
      </button>
    </div>

    <!-- Storage-attachment dialogs. Each + menu entry opens one of
         these in place, so the user never leaves the file manager
         to mount a disk or connect a cloud provider. -->
    <QuickDiskPickerDialog v-model:visible="quickDiskVisible" />
    <ConnectCloudProviderDialog
      v-model:visible="cloudDialogVisible"
      :provider="cloudDialogProvider"
    />
    <ConnectNetworkShareDialog v-model:visible="networkDialogVisible" />
  </nav>
</template>
