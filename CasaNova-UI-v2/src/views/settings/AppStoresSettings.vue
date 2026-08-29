<script setup lang="ts">
// AppStoresSettings — manage the app *sources* (stores) that feed the
// unified Library catalog. Multiple sources merge into one catalog,
// ordered by priority (lower number = higher precedence on conflicts).
//
// Backend contract lives in CasaNova-AppManagement:
//   GET/POST/PUT/DELETE /v2/app_management/stores (+ /:id/sync)
// All mutations go through the library Pinia store actions
// (addStore/removeStore/toggleStore/reprioritize/sync), which wrap the
// api-client and re-fetch the authoritative store list on success. The
// backend owns SSRF validation, default-store protection, and merge
// ordering — this page is purely the operator surface.

import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import dayjs from 'dayjs'
import relativeTime from 'dayjs/plugin/relativeTime'
import InputText from 'primevue/inputtext'
import Button from 'primevue/button'
import ToggleSwitch from 'primevue/toggleswitch'
import Dialog from 'primevue/dialog'
import AppIcon from '@/components/ui/AppIcon.vue'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { useLibraryStore } from '@/stores/library'
import { useInstallProgressStore } from '@/stores/installProgress'
import type { StoreEntry } from '@/api/library'

dayjs.extend(relativeTime)

const { t } = useI18n()
const library = useLibraryStore()
const toasts = useInstallProgressStore()

// Built-in sources that ship with every install. They must never be
// deletable from the UI — the backend also rejects it, but disabling the
// button keeps the intent obvious. Both defaults are covered so neither
// the local "default" nor the compat "casaos-official" baseline can be
// yanked out from under the catalog.
const PROTECTED_IDS = new Set(['default', 'casaos-official'])

// Sorted view: by priority asc, then name — mirrors how the backend
// merges the catalog so the list reads top-to-bottom as precedence.
const sortedStores = computed(() =>
  [...library.stores].sort((a, b) => a.priority - b.priority || a.name.localeCompare(b.name)),
)

onMounted(() => {
  library.refreshStores()
})

function isProtected(s: StoreEntry): boolean {
  return PROTECTED_IDS.has(s.id) || s.type === 'local'
}

function lastSyncedLabel(s: StoreEntry): string {
  if (!s.last_synced) return t('settings.appStores.never_synced')
  return dayjs(s.last_synced).fromNow()
}

// ----- Add remote source form -----
const newName = ref('')
const newUrl = ref('')
const adding = ref(false)
const urlError = ref('')

function validUrl(u: string): boolean {
  try {
    const parsed = new URL(u.trim())
    return parsed.protocol === 'http:' || parsed.protocol === 'https:'
  } catch {
    return false
  }
}

const canAdd = computed(() => newName.value.trim().length > 0 && newUrl.value.trim().length > 0)

async function addSource() {
  urlError.value = ''
  if (!validUrl(newUrl.value)) {
    urlError.value = t('settings.appStores.url_invalid')
    return
  }
  adding.value = true
  const name = newName.value.trim()
  const err = await library.addStore({
    name,
    type: 'remote',
    url: newUrl.value.trim(),
    enabled: true,
  })
  adding.value = false
  if (err) {
    toasts.pushToast('error', err)
    return
  }
  toasts.pushToast('success', t('settings.appStores.added', { name }))
  newName.value = ''
  newUrl.value = ''
}

// ----- Toggle enabled -----
async function onToggle(s: StoreEntry, enabled: boolean) {
  const err = await library.toggleStore(s.id, enabled)
  if (err) toasts.pushToast('error', err)
}

// ----- Reprioritize (swap with neighbour in the sorted list) -----
async function bumpPriority(s: StoreEntry, delta: number) {
  const err = await library.reprioritize(s.id, s.priority + delta)
  if (err) toasts.pushToast('error', err)
}

// ----- Sync -----
const syncingId = ref<string | null>(null)
async function onSync(s: StoreEntry) {
  syncingId.value = s.id
  const err = await library.sync(s.id)
  syncingId.value = null
  if (err) {
    toasts.pushToast('error', err)
    return
  }
  toasts.pushToast('success', t('settings.appStores.synced', { name: s.name }))
}

// ----- Delete -----
const deleteTarget = ref<StoreEntry | null>(null)
const deleting = ref(false)
function confirmDelete(s: StoreEntry) {
  deleteTarget.value = s
}
async function doDelete() {
  if (!deleteTarget.value) return
  deleting.value = true
  const err = await library.removeStore(deleteTarget.value.id)
  deleting.value = false
  if (err) {
    toasts.pushToast('error', err)
    return
  }
  deleteTarget.value = null
}
</script>

<template>
  <SettingsPage
    icon="Store"
    :title="t('settings.appStores.title')"
    :description="t('settings.appStores.description')"
  >
    <!-- Sources list. The former "How it works" card restated the page
         description verbatim (merge order, priority wins); only the
         compatibility note carried information, so it lives here as one line. -->
    <SettingsCard :title="t('settings.appStores.sources_title')">
      <div class="mb-3 flex items-start gap-2 text-xs text-[var(--cn-text-muted)]">
        <AppIcon name="Store" :size="14" class="mt-0.5 shrink-0 text-blue-500" />
        <span>{{ t('settings.appStores.compat_note') }}</span>
      </div>
      <!-- Empty state -->
      <div
        v-if="sortedStores.length === 0"
        class="rounded-lg border border-dashed border-[var(--cn-border)] p-6 text-center text-xs text-[var(--cn-text-muted)]"
      >
        {{ t('settings.appStores.empty') }}
      </div>

      <div v-else class="flex flex-col gap-2">
        <div
          v-for="(s, idx) in sortedStores"
          :key="s.id"
          class="flex flex-col gap-3 rounded-lg border border-[var(--cn-border)] px-3 py-3 sm:flex-row sm:items-center sm:gap-4"
          :class="s.enabled ? '' : 'opacity-60'"
        >
          <!-- Identity: icon + name + type badge + url -->
          <div class="flex min-w-0 flex-1 items-center gap-3">
            <div
              class="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg"
              :class="s.type === 'remote' ? 'bg-blue-500/10 text-blue-500' : 'bg-emerald-500/10 text-emerald-500'"
            >
              <AppIcon :name="s.type === 'remote' ? 'Globe' : 'HardDrive'" :size="16" />
            </div>
            <div class="min-w-0">
              <div class="flex items-center gap-2">
                <span class="truncate text-sm font-medium text-[var(--cn-text)]">{{ s.name }}</span>
                <span
                  class="rounded-full px-2 py-0.5 text-[10px] font-medium"
                  :class="s.type === 'remote'
                    ? 'bg-blue-500/15 text-blue-500'
                    : 'bg-emerald-500/15 text-emerald-500'"
                >
                  {{ s.type === 'remote' ? t('settings.appStores.type_remote') : t('settings.appStores.type_local') }}
                </span>
              </div>
              <div
                v-if="s.type === 'remote' && s.url"
                class="truncate font-mono text-xs text-[var(--cn-text-muted)]"
                :title="s.url"
              >
                {{ s.url }}
              </div>
              <div class="mt-0.5 flex items-center gap-1 text-[11px] text-[var(--cn-text-muted)]">
                <AppIcon name="RefreshCw" :size="11" />
                <span>{{ t('settings.appStores.last_synced') }}: {{ lastSyncedLabel(s) }}</span>
              </div>
            </div>
          </div>

          <!-- Priority controls -->
          <div class="flex items-center gap-1">
            <span class="mr-1 text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">
              {{ t('settings.appStores.priority') }}
            </span>
            <button
              type="button"
              class="rounded p-1 text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)] disabled:opacity-30 disabled:hover:bg-transparent"
              :disabled="idx === 0"
              :aria-label="t('settings.appStores.priority_up')"
              @click="bumpPriority(s, -1)"
            >
              <AppIcon name="ArrowUp" :size="14" />
            </button>
            <span class="w-6 text-center font-mono text-xs text-[var(--cn-text)]">{{ s.priority }}</span>
            <button
              type="button"
              class="rounded p-1 text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)] disabled:opacity-30 disabled:hover:bg-transparent"
              :disabled="idx === sortedStores.length - 1"
              :aria-label="t('settings.appStores.priority_down')"
              @click="bumpPriority(s, 1)"
            >
              <AppIcon name="ArrowDown" :size="14" />
            </button>
          </div>

          <!-- Actions: sync + enable toggle + delete -->
          <div class="flex items-center gap-2">
            <Button
              :label="syncingId === s.id ? t('settings.appStores.syncing') : t('settings.appStores.sync')"
              size="small"
              severity="secondary"
              :loading="syncingId === s.id"
              :disabled="syncingId === s.id"
              @click="onSync(s)"
            >
              <template #icon>
                <AppIcon name="RefreshCw" :size="13" />
              </template>
            </Button>

            <ToggleSwitch
              :model-value="s.enabled"
              :aria-label="s.enabled ? t('settings.appStores.enabled') : t('settings.appStores.disabled')"
              @update:model-value="(v: boolean) => onToggle(s, v)"
            />

            <button
              type="button"
              class="rounded p-1.5 text-[var(--cn-text-muted)] enabled:hover:bg-red-500/10 enabled:hover:text-red-500 disabled:cursor-not-allowed disabled:opacity-30"
              :disabled="isProtected(s)"
              :title="isProtected(s) ? t('settings.appStores.delete_default_hint') : t('settings.appStores.delete')"
              :aria-label="t('settings.appStores.delete')"
              @click="confirmDelete(s)"
            >
              <AppIcon name="Trash2" :size="15" />
            </button>
          </div>
        </div>
      </div>
    </SettingsCard>

    <!-- Add remote source -->
    <SettingsCard
      :title="t('settings.appStores.add_title')"
      :description="t('settings.appStores.add_desc')"
    >
      <div class="flex flex-col gap-3">
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div>
            <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
              {{ t('settings.appStores.name_label') }}
            </label>
            <InputText
              v-model="newName"
              :placeholder="t('settings.appStores.name_placeholder')"
              class="w-full"
            />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
              {{ t('settings.appStores.url_label') }}
            </label>
            <InputText
              v-model="newUrl"
              :placeholder="t('settings.appStores.url_placeholder')"
              class="w-full font-mono text-xs"
              @keyup.enter="canAdd && addSource()"
            />
          </div>
        </div>
        <div v-if="urlError" class="text-xs text-red-500">{{ urlError }}</div>
        <div>
          <Button
            :label="adding ? t('settings.appStores.adding') : t('settings.appStores.add_button')"
            size="small"
            :loading="adding"
            :disabled="!canAdd || adding"
            @click="addSource"
          >
            <template #icon>
              <AppIcon name="Plus" :size="13" />
            </template>
          </Button>
        </div>
      </div>
    </SettingsCard>

    <!-- Delete confirmation -->
    <Dialog
      :visible="deleteTarget !== null"
      :header="t('settings.appStores.confirm_delete_title')"
      :style="{ width: '400px' }"
      modal
      @update:visible="(v: boolean) => { if (!v) deleteTarget = null }"
    >
      <p class="text-sm text-[var(--cn-text-muted)]">
        {{ t('settings.appStores.confirm_delete', { name: deleteTarget?.name ?? '' }) }}
      </p>
      <template #footer>
        <Button
          :label="t('common.cancel')"
          size="small"
          severity="secondary"
          @click="deleteTarget = null"
        />
        <Button
          :label="t('settings.appStores.delete')"
          size="small"
          severity="danger"
          :loading="deleting"
          @click="doDelete"
        />
      </template>
    </Dialog>
  </SettingsPage>
</template>
