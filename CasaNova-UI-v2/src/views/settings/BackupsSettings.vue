<script setup lang="ts">
// Backups settings — system backup & restore + cloud backup via Rclone.
//
// Creates tar.gz backups of CasaNova system configuration (config
// files, databases, app manifest). Does NOT back up Docker data or
// user files. Lets users download, restore, or delete backups.
//
// Cloud backup syncs selected paths to remote cloud storage using
// rclone (Google Drive, Dropbox, S3, OneDrive, SFTP, etc.).

import { ref, onMounted, onUnmounted, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import Button from 'primevue/button'
import Dialog from 'primevue/dialog'
import InputText from 'primevue/inputtext'
import Select from 'primevue/select'
import dayjs from 'dayjs'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import client from '@/api/client'
import { safeCall } from '@/api/safeCall'
import { useInstallProgressStore } from '@/stores/installProgress'
import { formatBytes } from '@/stores/files'

const { t } = useI18n()
const toasts = useInstallProgressStore()

// ---- Local Backup ----

interface BackupEntry {
  filename: string
  size: number
  created: string
  path: string
}

const backups = ref<BackupEntry[]>([])
const creating = ref(false)
const createSuccess = ref<BackupEntry | null>(null)
const loading = ref(false)

// Restore dialog state
const restoreDialogVisible = ref(false)
const restoreTarget = ref<BackupEntry | null>(null)
const restoring = ref(false)

// Delete dialog state
const deleteDialogVisible = ref(false)
const deleteTarget = ref<BackupEntry | null>(null)
const deleting = ref(false)

async function fetchBackups() {
  loading.value = true
  const result = await safeCall(
    () => client.get('/v1/sys/backup/list').then((r) => r.data),
    () => ({ success: 200, data: [] }),
  )
  if (!result.error && result.data?.data) {
    backups.value = result.data.data
  }
  loading.value = false
}

async function createBackup() {
  creating.value = true
  createSuccess.value = null
  const result = await safeCall(
    () => client.post('/v1/sys/backup/create').then((r) => r.data),
  )
  if (!result.error && result.data?.data) {
    createSuccess.value = result.data.data
    await fetchBackups()
  }
  creating.value = false
}

async function downloadBackup(backup: BackupEntry) {
  // Wave 34-Y: previously `window.open()`ed the download URL in a new
  // tab. That path relies on the browser's credentialed session
  // (cookies) to authenticate, but CasaNova uses bearer JWTs from
  // localStorage — the new tab had no Authorization header, so every
  // download returned 401. The bugfix attempt people would reach for
  // ("append token to query string") leaks the access token into
  // browser history and gateway access logs, and is the identical
  // failure mode we just fixed on the file browser. Instead, stream
  // the tarball via the authenticated axios client and trigger the
  // download from a blob URL tied to the current document.
  try {
    const res = await client.get('/v1/sys/backup/download', {
      params: { filename: backup.filename },
      responseType: 'blob',
    })
    const url = URL.createObjectURL(res.data as Blob)
    const a = document.createElement('a')
    a.href = url
    a.download = backup.filename
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  } catch (err) {
    console.error('downloadBackup: failed', err)
  }
}

function openRestoreDialog(backup: BackupEntry) {
  restoreTarget.value = backup
  restoreDialogVisible.value = true
}

async function confirmRestore() {
  if (!restoreTarget.value) return
  restoring.value = true
  const result = await safeCall(
    () =>
      client
        .post('/v1/sys/backup/restore', { filename: restoreTarget.value!.filename })
        .then((r) => r.data),
  )
  restoring.value = false
  restoreDialogVisible.value = false
  if (result.error) {
    toasts.pushToast('error', result.error)
  } else {
    toasts.pushToast('success', t('settings.backups.restore_started'))
  }
}

function openDeleteDialog(backup: BackupEntry) {
  deleteTarget.value = backup
  deleteDialogVisible.value = true
}

async function confirmDelete() {
  if (!deleteTarget.value) return
  deleting.value = true
  const result = await safeCall(
    () =>
      client
        .delete('/v1/sys/backup', { data: { filename: deleteTarget.value!.filename } })
        .then((r) => r.data),
  )
  deleting.value = false
  deleteDialogVisible.value = false
  if (result.error) {
    toasts.pushToast('error', result.error)
  }
  await fetchBackups()
}

// ---- Cloud Backup ----

interface CloudRemote {
  name: string
  type: string
  configured: boolean
  last_sync?: string
}

interface CloudProvider {
  type: string
  name: string
  icon: string
}

interface CloudStatus {
  rclone_installed: boolean
  remotes: CloudRemote[]
  sync_paths: string[]
  schedule: string
  remote_path: string
  last_sync: string
  live_remotes: string[]
}

interface CloudSyncStatus {
  running: boolean
  progress: string
  started_at?: string
  last_error?: string
}

const cloudStatus = ref<CloudStatus | null>(null)
const cloudProviders = ref<CloudProvider[]>([])
const cloudSyncStatus = ref<CloudSyncStatus>({ running: false, progress: '' })
const cloudLoading = ref(false)

// Add remote dialog
const addRemoteDialogVisible = ref(false)
const addRemoteStep = ref(1)
const selectedProvider = ref<CloudProvider | null>(null)
const remoteName = ref('')
const remoteConfig = ref<Record<string, string>>({})
const addingRemote = ref(false)
const testResult = ref<{ ok: boolean; message: string } | null>(null)

// Remove remote dialog
const removeRemoteDialogVisible = ref(false)
const removeRemoteTarget = ref<CloudRemote | null>(null)
const removingRemote = ref(false)

// Config editing
const editSyncPaths = ref<string[]>([])
const editRemotePath = ref('')
const editSchedule = ref('manual')
const savingConfig = ref(false)
const configSaved = ref(false)
const customPath = ref('')

// Sync polling
let syncPollTimer: ReturnType<typeof setInterval> | null = null

// OAuth providers that need terminal setup
const oauthProviders = ['drive', 'dropbox', 'onedrive']

const scheduleOptions = computed(() => [
  { label: t('settings.backups.cloud_schedule_manual'), value: 'manual' },
  { label: t('settings.backups.cloud_schedule_daily'), value: 'daily' },
  { label: t('settings.backups.cloud_schedule_weekly'), value: 'weekly' },
])

// S3 config fields
const s3Fields = [
  { key: 'access_key_id', label: 'Access Key ID' },
  { key: 'secret_access_key', label: 'Secret Access Key' },
  { key: 'region', label: 'Region' },
  { key: 'bucket', label: 'Bucket' },
]

// SFTP config fields
const sftpFields = [
  { key: 'host', label: 'Host' },
  { key: 'user', label: 'Username' },
  { key: 'pass', label: 'Password' },
  { key: 'port', label: 'Port' },
]

// B2 config fields
const b2Fields = [
  { key: 'account', label: 'Account ID' },
  { key: 'key', label: 'Application Key' },
  { key: 'bucket', label: 'Bucket' },
]

function getConfigFields(providerType: string) {
  switch (providerType) {
    case 's3': return s3Fields
    case 'sftp': return sftpFields
    case 'b2': return b2Fields
    default: return []
  }
}

// Common backup paths — canonical /etc/casanova first; legacy
// /etc/casaos kept so users upgrading from CasaOS still see their
// existing config tree as a suggested backup target.
const commonPaths = ['/etc/casanova', '/var/lib/casanova/db', '/etc/casaos', '/var/lib/casaos/db', '/var/lib/casaos/conf']

async function fetchCloudStatus() {
  cloudLoading.value = true
  const result = await safeCall(
    () => client.get('/v1/sys/cloud/status').then((r) => r.data),
  )
  if (!result.error && result.data?.data) {
    cloudStatus.value = result.data.data
    editSyncPaths.value = [...(result.data.data.sync_paths || [])]
    editRemotePath.value = result.data.data.remote_path || 'CasaNova-Backup'
    editSchedule.value = result.data.data.schedule || 'manual'
  }
  cloudLoading.value = false
}

async function fetchCloudProviders() {
  const result = await safeCall(
    () => client.get('/v1/sys/cloud/providers').then((r) => r.data),
  )
  if (!result.error && result.data?.data) {
    cloudProviders.value = result.data.data
  }
}

async function fetchSyncStatus() {
  const result = await safeCall(
    () => client.get('/v1/sys/cloud/sync/status').then((r) => r.data),
  )
  if (!result.error && result.data?.data) {
    cloudSyncStatus.value = result.data.data
    if (!result.data.data.running && syncPollTimer) {
      clearInterval(syncPollTimer)
      syncPollTimer = null
      // Refresh cloud status to pick up new last_sync times.
      fetchCloudStatus()
    }
  }
}

function startSyncPolling() {
  if (syncPollTimer) return
  syncPollTimer = setInterval(fetchSyncStatus, 2000)
}

function openAddRemoteDialog() {
  addRemoteStep.value = 1
  selectedProvider.value = null
  remoteName.value = ''
  remoteConfig.value = {}
  testResult.value = null
  addRemoteDialogVisible.value = true
}

function selectProvider(provider: CloudProvider) {
  selectedProvider.value = provider
  remoteConfig.value = {}
  addRemoteStep.value = 2
}

async function addRemote() {
  if (!selectedProvider.value) return
  addingRemote.value = true
  testResult.value = null

  const result = await safeCall(
    () =>
      client
        .post('/v1/sys/cloud/remotes', {
          name: remoteName.value,
          type: selectedProvider.value!.type,
          config: remoteConfig.value,
        })
        .then((r) => r.data),
  )

  addingRemote.value = false

  if (!result.error && result.data?.data) {
    testResult.value = {
      ok: result.data.data.test_ok,
      message: result.data.data.test_message,
    }
    addRemoteStep.value = 3
    await fetchCloudStatus()
  } else {
    testResult.value = {
      ok: false,
      message: result.error || 'Failed to add remote',
    }
    addRemoteStep.value = 3
  }
}

function openRemoveRemoteDialog(remote: CloudRemote) {
  removeRemoteTarget.value = remote
  removeRemoteDialogVisible.value = true
}

async function confirmRemoveRemote() {
  if (!removeRemoteTarget.value) return
  removingRemote.value = true
  const result = await safeCall(
    () =>
      client
        .delete(`/v1/sys/cloud/remotes/${removeRemoteTarget.value!.name}`)
        .then((r) => r.data),
  )
  removingRemote.value = false
  removeRemoteDialogVisible.value = false
  if (result.error) {
    toasts.pushToast('error', result.error)
  }
  await fetchCloudStatus()
}

async function syncNow(remote: CloudRemote) {
  await safeCall(
    () =>
      client
        .post('/v1/sys/cloud/sync', {
          remote: remote.name,
          paths: editSyncPaths.value,
          remote_path: editRemotePath.value,
        })
        .then((r) => r.data),
  )
  cloudSyncStatus.value = { running: true, progress: 'Starting...', started_at: new Date().toISOString() }
  startSyncPolling()
}

function toggleSyncPath(path: string) {
  const idx = editSyncPaths.value.indexOf(path)
  if (idx >= 0) {
    editSyncPaths.value.splice(idx, 1)
  } else {
    editSyncPaths.value.push(path)
  }
}

function addCustomPath() {
  const p = customPath.value.trim()
  if (p && !editSyncPaths.value.includes(p)) {
    editSyncPaths.value.push(p)
  }
  customPath.value = ''
}

function removeCustomPath(path: string) {
  editSyncPaths.value = editSyncPaths.value.filter((p) => p !== path)
}

async function saveCloudConfig() {
  savingConfig.value = true
  configSaved.value = false
  const result = await safeCall(
    () =>
      client
        .post('/v1/sys/cloud/config', {
          sync_paths: editSyncPaths.value,
          schedule: editSchedule.value,
          remote_path: editRemotePath.value,
        })
        .then((r) => r.data),
  )
  savingConfig.value = false
  if (result.error) {
    // The banner used to flip to "Saved" unconditionally, so a rejected
    // config looked identical to a stored one — the worst possible lie for a
    // backup schedule, which nobody checks again until they need a restore.
    toasts.pushToast('error', result.error)
    return
  }
  configSaved.value = true
  setTimeout(() => { configSaved.value = false }, 3000)
}

function getProviderDisplayName(type: string): string {
  const p = cloudProviders.value.find((p) => p.type === type)
  return p ? p.name : type
}

function getProviderIcon(type: string): string {
  // Icon NAME (rendered through AppIcon so it follows the user's icon pack).
  switch (type) {
    case 'sftp': return 'Server'
    default: return 'Cloud'
  }
}

function formatDate(iso: string): string {
  return dayjs(iso).format('MMM D, YYYY HH:mm')
}

onMounted(() => {
  fetchBackups()
  fetchCloudStatus()
  fetchCloudProviders()
  fetchSyncStatus()
})

onUnmounted(() => {
  if (syncPollTimer) {
    clearInterval(syncPollTimer)
    syncPollTimer = null
  }
})
</script>

<template>
  <SettingsPage
    icon="Archive"
    :title="t('settings.backups.title')"
    :description="t('settings.backups.description')"
  >
    <!-- Create Backup Card -->
    <SettingsCard :title="t('settings.backups.create_title')">
      <div class="flex flex-col gap-4">
        <p class="text-sm text-[var(--cn-text-muted)]">
          {{ t('settings.backups.create_desc') }}
        </p>

        <div class="rounded-lg border border-[var(--cn-border)] bg-[var(--cn-fill-quaternary)] p-3">
          <div class="mb-2 text-xs font-medium text-[var(--cn-text)]">
            {{ t('settings.backups.includes') }}
          </div>
          <ul class="space-y-1 text-xs text-[var(--cn-text-muted)]">
            <li class="flex items-center gap-2">
              <AppIcon name="HardDrive" :size="12" />
              {{ t('settings.backups.includes_configs') }}
            </li>
            <li class="flex items-center gap-2">
              <AppIcon name="FileArchive" :size="12" />
              {{ t('settings.backups.includes_databases') }}
            </li>
            <li class="flex items-center gap-2">
              <AppIcon name="Clock" :size="12" />
              {{ t('settings.backups.includes_manifest') }}
            </li>
          </ul>
          <p class="mt-2 text-xs italic text-[var(--cn-text-muted)]">
            {{ t('settings.backups.excludes_note') }}
          </p>
        </div>

        <div class="flex items-center gap-3">
          <Button
            :label="creating ? t('settings.backups.creating') : t('settings.backups.create_button')"
            :loading="creating"
            :disabled="creating"
            @click="createBackup"
          >
            <template #icon>
              <AppIcon name="Plus" :size="14" />
            </template>
          </Button>

          <span
            v-if="createSuccess"
            class="text-sm text-emerald-500"
          >
            {{ t('settings.backups.created_success') }} &mdash;
            {{ createSuccess.filename }}
            ({{ formatBytes(createSuccess.size) }})
          </span>
        </div>
      </div>
    </SettingsCard>

    <!-- Backup History Card -->
    <SettingsCard :title="t('settings.backups.history_title')">
      <p class="mb-3 text-sm text-[var(--cn-text-muted)]">
        {{ t('settings.backups.history_desc') }}
      </p>

      <div class="flex flex-col gap-3">
        <div
          v-for="backup in backups"
          :key="backup.filename"
          class="flex items-center justify-between gap-4 rounded-lg border border-[var(--cn-border)] p-4"
        >
          <div class="flex min-w-0 flex-1 items-center gap-3">
            <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-blue-500/10 text-blue-500">
              <AppIcon name="FileArchive" :size="18" />
            </div>
            <div class="min-w-0 flex-1">
              <div class="truncate font-mono text-sm font-medium text-[var(--cn-text)]">
                {{ backup.filename }}
              </div>
              <div class="mt-1 flex flex-wrap gap-3 text-xs text-[var(--cn-text-muted)]">
                <span class="inline-flex items-center gap-1">
                  <AppIcon name="Clock" :size="11" />
                  {{ formatDate(backup.created) }}
                </span>
                <span>{{ formatBytes(backup.size) }}</span>
              </div>
            </div>
          </div>

          <div class="flex shrink-0 items-center gap-2">
            <Button
              :label="t('settings.backups.download')"
              size="small"
              severity="secondary"
              @click="downloadBackup(backup)"
            >
              <template #icon>
                <AppIcon name="Download" :size="13" />
              </template>
            </Button>
            <Button
              :label="t('settings.backups.restore')"
              size="small"
              severity="secondary"
              @click="openRestoreDialog(backup)"
            >
              <template #icon>
                <AppIcon name="RotateCcw" :size="13" />
              </template>
            </Button>
            <Button
              :label="t('settings.backups.delete')"
              size="small"
              severity="danger"
              text
              @click="openDeleteDialog(backup)"
            >
              <template #icon>
                <AppIcon name="Trash2" :size="13" />
              </template>
            </Button>
          </div>
        </div>

        <!-- Empty state -->
        <div
          v-if="backups.length === 0 && !loading"
          class="py-8 text-center"
        >
          <AppIcon name="Archive" :size="32" class="mx-auto mb-2 text-[var(--cn-text-muted)]" />
          <div class="mt-1 text-sm text-[var(--cn-text-muted)]">
            {{ t('settings.backups.no_backups') }}
          </div>
        </div>
      </div>
    </SettingsCard>

    <!-- ============================================ -->
    <!-- Cloud Backup Section                         -->
    <!-- ============================================ -->

    <SettingsCard :title="t('settings.backups.cloud_title')">
      <p class="mb-4 text-sm text-[var(--cn-text-muted)]">
        {{ t('settings.backups.cloud_desc') }}
      </p>

      <!-- Rclone not installed -->
      <div
        v-if="cloudStatus && !cloudStatus.rclone_installed"
        class="flex items-start gap-3 rounded-lg border border-amber-500/30 bg-amber-500/10 p-4"
      >
        <AppIcon name="AlertTriangle" :size="18" class="mt-0.5 shrink-0 text-amber-500" />
        <div>
          <p class="text-sm font-medium text-[var(--cn-text)]">
            {{ t('settings.backups.cloud_not_installed') }}
          </p>
          <p class="mt-1 font-mono text-xs text-[var(--cn-text-muted)]">
            curl https://rclone.org/install.sh | sudo bash
          </p>
        </div>
      </div>

      <!-- Rclone installed — show remotes -->
      <template v-if="cloudStatus && cloudStatus.rclone_installed">
        <!-- Configured remotes list -->
        <div class="flex flex-col gap-3">
          <div
            v-for="remote in cloudStatus.remotes"
            :key="remote.name"
            class="flex items-center justify-between gap-4 rounded-lg border border-[var(--cn-border)] p-4"
          >
            <div class="flex min-w-0 flex-1 items-center gap-3">
              <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-sky-500/10 text-sky-500">
                <AppIcon :name="getProviderIcon(remote.type)" :size="18" />
              </div>
              <div class="min-w-0 flex-1">
                <div class="font-medium text-sm text-[var(--cn-text)]">
                  {{ remote.name }}
                  <span class="ml-2 text-xs text-[var(--cn-text-muted)]">{{ getProviderDisplayName(remote.type) }}</span>
                </div>
                <div class="mt-1 text-xs text-[var(--cn-text-muted)]">
                  <span v-if="remote.last_sync" class="inline-flex items-center gap-1">
                    <AppIcon name="Clock" :size="11" />
                    {{ t('settings.backups.cloud_last_sync') }}: {{ formatDate(remote.last_sync) }}
                  </span>
                  <span v-else class="inline-flex items-center gap-1">
                    <AppIcon name="Clock" :size="11" />
                    {{ t('settings.backups.cloud_never_synced') }}
                  </span>
                </div>
              </div>
            </div>

            <div class="flex shrink-0 items-center gap-2">
              <Button
                :label="cloudSyncStatus.running ? t('settings.backups.cloud_syncing') : t('settings.backups.cloud_sync_now')"
                size="small"
                :disabled="cloudSyncStatus.running"
                @click="syncNow(remote)"
              >
                <template #icon>
                  <AppIcon name="CloudUpload" :size="13" />
                </template>
              </Button>
              <Button
                :label="t('settings.backups.cloud_remove_remote')"
                size="small"
                severity="danger"
                text
                @click="openRemoveRemoteDialog(remote)"
              >
                <template #icon>
                  <AppIcon name="Trash2" :size="13" />
                </template>
              </Button>
            </div>
          </div>

          <!-- Empty state -->
          <div
            v-if="cloudStatus.remotes.length === 0"
            class="py-6 text-center"
          >
            <AppIcon name="Cloud" :size="32" class="mx-auto mb-2 text-[var(--cn-text-muted)]" />
            <div class="mt-1 text-sm text-[var(--cn-text-muted)]">
              {{ t('settings.backups.cloud_no_remotes') }}
            </div>
          </div>

          <!-- Add remote button -->
          <div class="mt-2">
            <Button
              :label="t('settings.backups.cloud_add_remote')"
              severity="secondary"
              @click="openAddRemoteDialog"
            >
              <template #icon>
                <AppIcon name="Plus" :size="14" />
              </template>
            </Button>
          </div>
        </div>

        <!-- Sync status banner -->
        <div
          v-if="cloudSyncStatus.running"
          class="mt-4 flex items-center gap-3 rounded-lg border border-sky-500/30 bg-sky-500/10 p-3"
        >
          <AppIcon name="RefreshCw" :size="16" class="shrink-0 animate-spin text-sky-500" />
          <div class="min-w-0 flex-1">
            <div class="text-sm font-medium text-[var(--cn-text)]">
              {{ t('settings.backups.cloud_sync_running') }}
            </div>
            <div class="mt-0.5 text-xs text-[var(--cn-text-muted)]">
              {{ cloudSyncStatus.progress }}
            </div>
          </div>
        </div>

        <!-- Sync config section -->
        <div
          v-if="cloudStatus.remotes.length > 0"
          class="mt-6 border-t border-[var(--cn-border)] pt-4"
        >
          <h4 class="mb-3 text-sm font-medium text-[var(--cn-text)]">
            {{ t('settings.backups.cloud_sync_paths') }}
          </h4>

          <!-- Common paths as toggleable chips -->
          <div class="flex flex-wrap gap-2">
            <button
              v-for="path in commonPaths"
              :key="path"
              class="inline-flex items-center gap-1.5 rounded-full border px-3 py-1.5 text-xs font-medium transition-colors"
              :class="
                editSyncPaths.includes(path)
                  ? 'border-sky-500/50 bg-sky-500/10 text-sky-600'
                  : 'border-[var(--cn-border)] bg-transparent text-[var(--cn-text-muted)] hover:border-[var(--cn-text-muted)]'
              "
              @click="toggleSyncPath(path)"
            >
              <AppIcon v-if="editSyncPaths.includes(path)" name="CheckCircle2" :size="12" />
              <AppIcon v-else name="HardDrive" :size="12" />
              {{ path }}
            </button>
          </div>

          <!-- Custom paths -->
          <div
            v-for="path in editSyncPaths.filter((p) => !commonPaths.includes(p))"
            :key="path"
            class="mt-2 inline-flex items-center gap-1.5 rounded-full border border-sky-500/50 bg-sky-500/10 px-3 py-1.5 text-xs font-medium text-sky-600"
          >
            <AppIcon name="FolderSync" :size="12" />
            {{ path }}
            <button class="ml-1 hover:text-red-500" @click="removeCustomPath(path)">
              <AppIcon name="Trash2" :size="10" />
            </button>
          </div>

          <!-- Add custom path -->
          <div class="mt-3 flex items-center gap-2">
            <InputText
              v-model="customPath"
              placeholder="/path/to/directory"
              class="!text-sm"
              size="small"
              @keyup.enter="addCustomPath"
            />
            <Button
              label="Add"
              size="small"
              severity="secondary"
              :disabled="!customPath.trim()"
              @click="addCustomPath"
            />
          </div>

          <!-- Remote path -->
          <div class="mt-4">
            <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
              {{ t('settings.backups.cloud_remote_path') }}
            </label>
            <InputText
              v-model="editRemotePath"
              class="!text-sm"
              size="small"
            />
          </div>

          <!-- Schedule -->
          <div class="mt-4">
            <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
              {{ t('settings.backups.cloud_schedule') }}
            </label>
            <Select
              v-model="editSchedule"
              :options="scheduleOptions"
              optionLabel="label"
              optionValue="value"
              class="w-48"
              size="small"
            />
          </div>

          <!-- Save config -->
          <div class="mt-4 flex items-center gap-3">
            <Button
              :label="t('settings.backups.cloud_save_config')"
              :loading="savingConfig"
              :disabled="savingConfig"
              @click="saveCloudConfig"
            >
              <template #icon>
                <AppIcon name="CheckCircle2" :size="14" />
              </template>
            </Button>
            <span v-if="configSaved" class="text-sm text-emerald-500">
              {{ t('common.save') }}d
            </span>
          </div>
        </div>
      </template>

      <!-- Loading state -->
      <div
        v-if="cloudLoading && !cloudStatus"
        class="py-6 text-center text-sm text-[var(--cn-text-muted)]"
      >
        {{ t('common.loading') }}
      </div>
    </SettingsCard>

    <!-- ============================================ -->
    <!-- Dialogs                                      -->
    <!-- ============================================ -->

    <!-- Restore Confirmation Dialog -->
    <Dialog
      v-model:visible="restoreDialogVisible"
      :header="t('settings.backups.restore_title')"
      modal
      :closable="!restoring"
      :style="{ width: '28rem' }"
    >
      <div class="flex flex-col gap-4">
        <div class="flex items-start gap-3 rounded-lg border border-amber-500/30 bg-amber-500/10 p-3">
          <AppIcon name="AlertTriangle" :size="18" class="mt-0.5 shrink-0 text-amber-500" />
          <p class="text-sm text-[var(--cn-text)]">
            {{ t('settings.backups.restore_warning') }}
          </p>
        </div>

        <div v-if="restoreTarget" class="rounded-lg border border-[var(--cn-border)] p-3">
          <div class="font-mono text-sm text-[var(--cn-text)]">
            {{ restoreTarget.filename }}
          </div>
          <div class="mt-1 text-xs text-[var(--cn-text-muted)]">
            {{ formatDate(restoreTarget.created) }} &middot; {{ formatBytes(restoreTarget.size) }}
          </div>
        </div>
      </div>

      <template #footer>
        <div class="flex justify-end gap-2">
          <Button
            :label="t('common.cancel')"
            severity="secondary"
            text
            :disabled="restoring"
            @click="restoreDialogVisible = false"
          />
          <Button
            :label="t('settings.backups.restore_confirm')"
            severity="warn"
            :loading="restoring"
            :disabled="restoring"
            @click="confirmRestore"
          >
            <template #icon>
              <AppIcon name="RotateCcw" :size="14" />
            </template>
          </Button>
        </div>
      </template>
    </Dialog>

    <!-- Delete Confirmation Dialog -->
    <Dialog
      v-model:visible="deleteDialogVisible"
      :header="t('settings.backups.delete_title')"
      modal
      :closable="!deleting"
      :style="{ width: '28rem' }"
    >
      <div class="flex flex-col gap-4">
        <div class="flex items-start gap-3 rounded-lg border border-red-500/30 bg-red-500/10 p-3">
          <AppIcon name="AlertTriangle" :size="18" class="mt-0.5 shrink-0 text-red-500" />
          <p class="text-sm text-[var(--cn-text)]">
            {{ t('settings.backups.delete_warning') }}
          </p>
        </div>

        <div v-if="deleteTarget" class="rounded-lg border border-[var(--cn-border)] p-3">
          <div class="font-mono text-sm text-[var(--cn-text)]">
            {{ deleteTarget.filename }}
          </div>
          <div class="mt-1 text-xs text-[var(--cn-text-muted)]">
            {{ formatDate(deleteTarget.created) }} &middot; {{ formatBytes(deleteTarget.size) }}
          </div>
        </div>
      </div>

      <template #footer>
        <div class="flex justify-end gap-2">
          <Button
            :label="t('common.cancel')"
            severity="secondary"
            text
            :disabled="deleting"
            @click="deleteDialogVisible = false"
          />
          <Button
            :label="t('settings.backups.delete_confirm')"
            severity="danger"
            :loading="deleting"
            :disabled="deleting"
            @click="confirmDelete"
          >
            <template #icon>
              <AppIcon name="Trash2" :size="14" />
            </template>
          </Button>
        </div>
      </template>
    </Dialog>

    <!-- Add Cloud Remote Dialog -->
    <Dialog
      v-model:visible="addRemoteDialogVisible"
      :header="t('settings.backups.cloud_add_remote')"
      modal
      :closable="!addingRemote"
      :style="{ width: '36rem' }"
    >
      <div class="flex flex-col gap-4">
        <!-- Step 1: Select provider -->
        <template v-if="addRemoteStep === 1">
          <p class="text-sm text-[var(--cn-text-muted)]">
            {{ t('settings.backups.cloud_select_provider') }}
          </p>
          <div class="grid grid-cols-2 gap-3 sm:grid-cols-3">
            <button
              v-for="provider in cloudProviders"
              :key="provider.type"
              class="flex flex-col items-center gap-2 rounded-lg border border-[var(--cn-border)] p-4 transition-colors hover:border-sky-500/50 hover:bg-sky-500/5"
              @click="selectProvider(provider)"
            >
              <AppIcon :name="getProviderIcon(provider.type)" :size="24" class="text-[var(--cn-text)]" />
              <span class="text-sm font-medium text-[var(--cn-text)]">{{ provider.name }}</span>
            </button>
          </div>
        </template>

        <!-- Step 2: Configure -->
        <template v-if="addRemoteStep === 2 && selectedProvider">
          <p class="text-sm text-[var(--cn-text-muted)]">
            {{ t('settings.backups.cloud_configure') }}: {{ selectedProvider.name }}
          </p>

          <!-- Remote name -->
          <div>
            <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">Name</label>
            <InputText
              v-model="remoteName"
              placeholder="my-backup"
              class="w-full !text-sm"
              size="small"
            />
          </div>

          <!-- OAuth hint for Drive/Dropbox/OneDrive -->
          <div
            v-if="oauthProviders.includes(selectedProvider.type)"
            class="flex items-start gap-3 rounded-lg border border-amber-500/30 bg-amber-500/10 p-3"
          >
            <AppIcon name="AlertTriangle" :size="16" class="mt-0.5 shrink-0 text-amber-500" />
            <p class="text-xs text-[var(--cn-text)]">
              {{ t('settings.backups.cloud_oauth_hint') }}
            </p>
          </div>

          <!-- Provider-specific config fields -->
          <template v-if="getConfigFields(selectedProvider.type).length > 0">
            <div
              v-for="field in getConfigFields(selectedProvider.type)"
              :key="field.key"
            >
              <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
                {{ field.label }}
              </label>
              <InputText
                v-model="remoteConfig[field.key]"
                class="w-full !text-sm"
                size="small"
                :type="field.key.includes('secret') || field.key === 'pass' || field.key === 'key' ? 'password' : 'text'"
              />
            </div>
          </template>

          <!-- Generic key-value for unknown providers -->
          <div
            v-if="getConfigFields(selectedProvider.type).length === 0 && !oauthProviders.includes(selectedProvider.type)"
            class="text-xs text-[var(--cn-text-muted)]"
          >
            No additional configuration needed. The remote will be created with default settings.
          </div>
        </template>

        <!-- Step 3: Test result -->
        <template v-if="addRemoteStep === 3">
          <div
            v-if="testResult"
            class="flex items-start gap-3 rounded-lg border p-4"
            :class="testResult.ok ? 'border-emerald-500/30 bg-emerald-500/10' : 'border-red-500/30 bg-red-500/10'"
          >
            <AppIcon
              v-if="testResult.ok"
              name="CheckCircle2"
              :size="18"
              class="mt-0.5 shrink-0 text-emerald-500"
            />
            <AppIcon
              v-else
              name="AlertTriangle"
              :size="18"
              class="mt-0.5 shrink-0 text-red-500"
            />
            <div>
              <p class="text-sm font-medium text-[var(--cn-text)]">
                {{ testResult.ok ? t('settings.backups.cloud_test_success') : t('settings.backups.cloud_test_failed') }}
              </p>
              <p v-if="testResult.message" class="mt-1 text-xs text-[var(--cn-text-muted)]">
                {{ testResult.message }}
              </p>
            </div>
          </div>
        </template>
      </div>

      <template #footer>
        <div class="flex justify-end gap-2">
          <Button
            v-if="addRemoteStep > 1 && addRemoteStep < 3"
            label="Back"
            severity="secondary"
            text
            @click="addRemoteStep--"
          />
          <Button
            :label="t('common.cancel')"
            severity="secondary"
            text
            :disabled="addingRemote"
            @click="addRemoteDialogVisible = false"
          />
          <Button
            v-if="addRemoteStep === 2"
            :label="t('settings.backups.cloud_test_connection')"
            :loading="addingRemote"
            :disabled="addingRemote || !remoteName.trim()"
            @click="addRemote"
          >
            <template #icon>
              <AppIcon name="CloudUpload" :size="14" />
            </template>
          </Button>
          <Button
            v-if="addRemoteStep === 3"
            label="Done"
            @click="addRemoteDialogVisible = false"
          />
        </div>
      </template>
    </Dialog>

    <!-- Remove Cloud Remote Dialog -->
    <Dialog
      v-model:visible="removeRemoteDialogVisible"
      :header="t('settings.backups.cloud_remove_remote')"
      modal
      :closable="!removingRemote"
      :style="{ width: '28rem' }"
    >
      <div class="flex flex-col gap-4">
        <p class="text-sm text-[var(--cn-text)]">
          {{ t('settings.backups.cloud_remove_confirm', { name: removeRemoteTarget?.name }) }}
        </p>
      </div>

      <template #footer>
        <div class="flex justify-end gap-2">
          <Button
            :label="t('common.cancel')"
            severity="secondary"
            text
            :disabled="removingRemote"
            @click="removeRemoteDialogVisible = false"
          />
          <Button
            :label="t('common.delete')"
            severity="danger"
            :loading="removingRemote"
            :disabled="removingRemote"
            @click="confirmRemoveRemote"
          >
            <template #icon>
              <AppIcon name="Trash2" :size="14" />
            </template>
          </Button>
        </div>
      </template>
    </Dialog>
  </SettingsPage>
</template>
