<script setup lang="ts">
// SharedFoldersView — second child of the /app/files route. Lists
// every folder the user has shared over SMB with the in-place
// "Share" action from the file browser's context menu, plus the
// network paths (Windows / Mac) each client needs to connect.
//
// Source: a small fixture for now. Real backend wiring reads from
// GET /v1/samba/shares and each row's URIs come from the same Samba
// config the file browser's ShareFolderDialog generates.

import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import AppIcon from '@/components/ui/AppIcon.vue'
import Button from 'primevue/button'
import { useInstallProgressStore } from '@/stores/installProgress'
import { safeCall } from '@/api/safeCall'
import client from '@/api/client'
import { listShares, type SambaShare } from '@/api/files'

const { t } = useI18n()
const router = useRouter()
const installs = useInstallProgressStore()

// Wave 34-Y: previously this view hard-coded a three-row fixture
// ("Media", "Documents", "Backups") and presented it as the live
// share list. A user with no SMB shares saw three shares anyway;
// a user with seven shares saw only the three fake ones; and every
// "Copy Windows path" button generated credentials for shares that
// did not exist on the box. The Samba backend exposes GET
// /v1/samba/shares and our api/files.ts already wraps it as
// `listShares()` — wire the real call on mount.
interface SharedFolder {
  id: string
  name: string
  path: string
  addedAt: string
}

const shares = ref<SharedFolder[]>([])

function mapShare(s: SambaShare): SharedFolder {
  return {
    id: s.id,
    name: s.name,
    path: s.path,
    addedAt: s.added_at,
  }
}

onMounted(async () => {
  try {
    const list = await listShares()
    shares.value = list.map(mapShare)
  } catch (err) {
    console.error('SharedFoldersView: failed to load shares', err)
    shares.value = []
  }
})

const host = computed(() => window.location.hostname || 'casanova.local')
function windowsPath(name: string): string {
  return `\\\\${host.value}\\${name}`
}
function macPath(name: string): string {
  return `smb://${host.value}/${name}`
}

const copied = ref<string | null>(null)
async function copy(key: string, text: string) {
  try {
    await navigator.clipboard.writeText(text)
    copied.value = key
    setTimeout(() => (copied.value = null), 1500)
  } catch {
    /* clipboard blocked */
  }
}

function openFolder(share: SharedFolder) {
  router.push(`/app/files`)
  // Route the file browser to the shared path via the store once
  // the navigation settles. The browser reads currentPath from the
  // store so this is enough to land the user in the right folder.
  setTimeout(() => {
    void (async () => {
      try {
        const { useFilesStore } = await import('@/stores/files')
        useFilesStore().cd(share.path)
      } catch {
        installs.pushToast('error', 'Failed to open folder')
      }
    })()
  }, 50)
}

async function unshare(share: SharedFolder) {
  const result = await safeCall(
    () => client.delete(`/v1/samba/shares/${share.id}`),
    () => undefined,
  )
  if (result.error && !result.fixture) {
    installs.pushToast('error', result.error)
    return
  }
  shares.value = shares.value.filter((s) => s.id !== share.id)
  installs.pushToast('info', t('files.shared_view.unshared', { name: share.name }))
}
</script>

<template>
  <div class="cn-widget flex h-full flex-col p-5">
    <!-- Header. Mirrors the breadcrumb row of FileBrowserView so the
         two sibling views feel like the same surface. Close button
         on the right returns to the file browser (the main files
         landing) — without it users inside the Shared view have no
         quick exit affordance in the content pane itself. -->
    <div class="mb-4 flex items-center gap-3">
      <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-blue-500/15 text-blue-500">
        <AppIcon name="Share2" :size="20" />
      </div>
      <div class="min-w-0 flex-1">
        <h1 class="text-lg font-semibold text-[var(--cn-text)]">
          {{ t('files.shared_view.title') }}
        </h1>
        <p class="text-xs text-[var(--cn-text-muted)]">
          {{ t('files.shared_view.description') }}
        </p>
      </div>
      <button
        type="button"
        class="flex h-8 w-8 shrink-0 items-center justify-center rounded-full text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
        :aria-label="t('common.close')"
        :title="t('common.close')"
        @click="router.push('/app/files')"
      >
        <AppIcon name="X" :size="16" />
      </button>
    </div>

    <div v-if="shares.length === 0" class="flex flex-1 items-center justify-center py-16 text-center">
      <div>
        <AppIcon name="Share2" :size="32" class="mx-auto text-[var(--cn-text-muted)]" />
        <p class="mt-2 text-sm text-[var(--cn-text)]">{{ t('files.shared_view.empty_title') }}</p>
        <p class="mt-1 text-xs text-[var(--cn-text-muted)]">
          {{ t('files.shared_view.empty_subtitle') }}
        </p>
      </div>
    </div>

    <div v-else class="flex flex-col gap-3">
      <div
        v-for="share in shares"
        :key="share.id"
        class="rounded-xl border border-[var(--cn-border)] p-4"
      >
        <div class="flex items-start justify-between gap-4">
          <button
            type="button"
            class="flex min-w-0 flex-1 items-start gap-3 text-left"
            @click="openFolder(share)"
          >
            <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-blue-500/10 text-blue-500">
              <AppIcon name="Folder" :size="20" />
            </div>
            <div class="min-w-0 flex-1">
              <div class="truncate font-medium text-[var(--cn-text)]">
                {{ share.name }}
              </div>
              <div class="truncate font-mono text-xs text-[var(--cn-text-muted)]">
                {{ share.path }}
              </div>
            </div>
          </button>
          <Button
            :label="t('files.shared_view.unshare')"
            severity="secondary"
            size="small"
            @click="unshare(share)"
          >
            <template #icon><AppIcon name="Trash2" :size="13" /></template>
          </Button>
        </div>

        <!-- Copyable paths -->
        <div class="mt-3 grid grid-cols-1 gap-2 sm:grid-cols-2">
          <div>
            <div class="mb-1 text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">
              {{ t('files.share.windows') }}
            </div>
            <button
              type="button"
              class="flex w-full items-center gap-2 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-2 py-1.5 text-left font-mono text-[11px] text-[var(--cn-text)] transition-colors hover:bg-[var(--cn-border)]"
              @click="copy(`win-${share.id}`, windowsPath(share.name))"
            >
              <AppIcon
                :name="copied === `win-${share.id}` ? 'Check' : 'Copy'"
                :size="12"
                :class="copied === `win-${share.id}` ? 'text-emerald-500' : 'text-[var(--cn-text-muted)]'"
              />
              <span class="truncate">{{ windowsPath(share.name) }}</span>
            </button>
          </div>
          <div>
            <div class="mb-1 text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">
              {{ t('files.share.mac') }}
            </div>
            <button
              type="button"
              class="flex w-full items-center gap-2 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-2 py-1.5 text-left font-mono text-[11px] text-[var(--cn-text)] transition-colors hover:bg-[var(--cn-border)]"
              @click="copy(`mac-${share.id}`, macPath(share.name))"
            >
              <AppIcon
                :name="copied === `mac-${share.id}` ? 'Check' : 'Copy'"
                :size="12"
                :class="copied === `mac-${share.id}` ? 'text-emerald-500' : 'text-[var(--cn-text-muted)]'"
              />
              <span class="truncate">{{ macPath(share.name) }}</span>
            </button>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
