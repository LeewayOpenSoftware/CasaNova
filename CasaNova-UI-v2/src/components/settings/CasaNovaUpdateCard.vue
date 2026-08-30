<script setup lang="ts">
// CasaNovaUpdateCard — the self-update surface for CasaNova itself (the
// UpdateManager engine's frontend). Extracted from AboutSettings so it can
// live on the dedicated Updates page alongside the OS (apt) updater. Renders
// a SettingsCard + the load-from-disk dialog; drives the updates store.

import { onMounted, onBeforeUnmount, computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import Button from 'primevue/button'
import Dialog from 'primevue/dialog'
import ToggleSwitch from 'primevue/toggleswitch'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { useUpdatesStore } from '@/stores/updates'
import { systemApi, updatesApi } from '@/api'
import { safeCall } from '@/api/safeCall'
import type { UpdateChannel, DiskMount } from '@/api/updates'

const { t } = useI18n()
const updates = useUpdatesStore()
const allowReboot = ref(false)
const diskUpdateVisible = ref(false)
const diskUpdateError = ref('')
const diskScanning = ref(false)
const diskMounts = ref<DiskMount[]>([])
const selectedPackagePath = ref<string | null>(null)
const uploadProgress = ref<{ loaded: number; total: number; name: string } | null>(null)
const rollbackInFlight = ref(false)
const buildVersion = ref<string>('—')
const displayVersion = ref<string>(import.meta.env.VITE_DISPLAY_VERSION || '26.10')
const buildChannel = computed(() => updates.status.channel || 'dev')

async function onRollback() {
  if (rollbackInFlight.value) return
  if (!window.confirm(t('settings.about.rollback_confirm'))) return
  rollbackInFlight.value = true
  try {
    await updates.rollback()
    setTimeout(() => { rollbackInFlight.value = false }, 5000)
  } catch (e) {
    rollbackInFlight.value = false
    diskUpdateError.value = String(e)
  }
}

onMounted(async () => {
  updates.startPolling()
  updates.loadChangelog()
  const result = await safeCall(() => systemApi.getCurrentVersion())
  if (result.data) buildVersion.value = String(result.data)
})
onBeforeUnmount(() => { updates.stopPolling() })

function formatBytes(n: number | null): string {
  if (!n) return '—'
  if (n >= 1024 * 1024 * 1024) return (n / 1024 / 1024 / 1024).toFixed(1) + ' GB'
  if (n >= 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB'
  return (n / 1024).toFixed(0) + ' KB'
}
function formatTimestamp(iso: string | null): string {
  if (!iso) return t('settings.about.never')
  try { return new Date(iso).toLocaleString() } catch { return iso }
}

const channels: Array<{ value: UpdateChannel; label: string }> = [
  { value: 'stable', label: 'channel_stable' },
  { value: 'beta', label: 'channel_beta' },
  { value: 'dev' as UpdateChannel, label: 'channel_dev' },
  { value: 'nightly', label: 'channel_nightly' },
]

async function onCheckNow() { await updates.checkNow(); await updates.loadChangelog() }
async function onInstall() { await updates.startUpdate(allowReboot.value) }
async function onCancel() { await updates.cancelUpdate() }
async function onChannelChange(c: UpdateChannel) { await updates.changeChannel(c) }

// Maintenance — clear the cached package (free space) and repair (reinstall the
// current version's files from cache). A cached package is present whenever the
// status carries a latestVersion.
const clearingCache = ref(false)
const repairing = ref(false)
const hasCachedPackage = computed(() => !!updates.status.latestVersion)
async function onClearCache() {
  if (!window.confirm(t('settings.about.clear_cache_confirm'))) return
  clearingCache.value = true
  await updates.clearCache()
  clearingCache.value = false
}
async function onRepair() {
  if (!window.confirm(t('settings.about.repair_confirm'))) return
  repairing.value = true
  await updates.repairSystem()
  repairing.value = false
}

const diskHasSearched = ref(false)
function openDiskUpdate() {
  diskUpdateError.value = ''; selectedPackagePath.value = null; diskMounts.value = []
  diskHasSearched.value = false; diskUpdateVisible.value = true
}
async function scanForPackages() {
  diskScanning.value = true; diskUpdateError.value = ''; diskHasSearched.value = true
  try {
    const result = await safeCall(() => updatesApi.scanDisks())
    diskScanning.value = false
    if (result.error) { diskUpdateError.value = result.error; return }
    if (!result.data || !result.data.mounts) { diskUpdateError.value = 'Unexpected response from scan endpoint'; return }
    diskMounts.value = result.data.mounts
  } catch (e) { diskScanning.value = false; diskUpdateError.value = String(e) }
}
const fileInputRef = ref<HTMLInputElement | null>(null)
function openFilePicker() { fileInputRef.value?.click() }
async function onFileSelected(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  input.value = ''
  diskScanning.value = true; diskUpdateError.value = ''; diskHasSearched.value = true
  diskMounts.value = []; uploadProgress.value = { loaded: 0, total: file.size, name: file.name }
  try {
    const result = await safeCall(() =>
      updatesApi.uploadPackage(file, (loaded, total) => { uploadProgress.value = { loaded, total, name: file.name } }),
    )
    uploadProgress.value = null; diskScanning.value = false
    if (result.error) { diskUpdateError.value = result.error; return }
    if (!result.data?.path) { diskUpdateError.value = 'Server did not return a package path'; return }
    diskMounts.value = [{
      mountPoint: t('settings.about.uploaded_file'), device: '', label: file.name,
      packages: [{ path: result.data.path, filename: file.name, version: result.data.version || '?', arch: result.data.arch || '', sizeBytes: file.size, modifiedAt: new Date().toISOString() }],
    }]
    selectedPackagePath.value = result.data.path
  } catch (e) { uploadProgress.value = null; diskScanning.value = false; diskUpdateError.value = String(e) }
}
function selectPackage(path: string) { selectedPackagePath.value = selectedPackagePath.value === path ? null : path }
function isUsbDevice(device: string): boolean { return device.startsWith('/dev/sd') || device.includes('usb') }
function formatPackageSize(bytes: number): string {
  if (bytes >= 1024 * 1024 * 1024) return (bytes / 1024 / 1024 / 1024).toFixed(1) + ' GB'
  if (bytes >= 1024 * 1024) return (bytes / 1024 / 1024).toFixed(1) + ' MB'
  return (bytes / 1024).toFixed(0) + ' KB'
}
function formatPackageDate(iso: string): string {
  try { return new Date(iso).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) } catch { return iso }
}
const totalPackages = computed(() => diskMounts.value.reduce((sum, m) => sum + m.packages.length, 0))
async function onApplyDiskUpdate() {
  if (!selectedPackagePath.value) return
  diskUpdateError.value = ''; diskUpdateVisible.value = false
  await updates.startUpdateFromDisk(selectedPackagePath.value, allowReboot.value)
}
</script>

<template>
  <!-- No card title/description: the page header already says "Updates" and
       the status line below says what this card is about. The version stamp
       moved next to the buttons so the card opens straight into content. -->
  <SettingsCard>
    <div class="flex flex-col gap-4">
      <div class="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div class="flex items-start gap-3">
          <div class="mt-0.5 shrink-0">
            <AppIcon v-if="!updates.isActive && !updates.status.updateAvailable" name="CheckCircle2" :size="22" class="text-emerald-500" />
            <AppIcon v-else-if="!updates.isActive && updates.status.updateAvailable" name="Download" :size="22" class="text-blue-500" />
            <AppIcon v-else-if="updates.status.stage !== 'failed'" name="Loader2" :size="22" class="animate-spin text-blue-500" />
            <AppIcon v-else name="AlertTriangle" :size="22" class="text-red-500" />
          </div>
          <div class="min-w-0 flex-1">
            <div class="text-sm font-medium text-[var(--cn-text)]">
              <span v-if="updates.isActive">{{ t(`settings.about.stage.${updates.status.stage}`) }}</span>
              <span v-else-if="updates.status.updateAvailable">{{ t('settings.about.update_available') }}</span>
              <span v-else>{{ t('settings.about.up_to_date') }}</span>
            </div>
            <!-- The backend message is English-only and, when idle, just
                 restates the localized line above ("CasaNova is up to date").
                 Show it only while an update runs, where it carries the real
                 phase detail the status line can't. -->
            <div v-if="updates.status.message && updates.isActive" class="mt-0.5 text-xs text-[var(--cn-text-muted)]">{{ updates.status.message }}</div>
            <div v-if="updates.status.updateAvailable && updates.status.latestVersion && !updates.isActive" class="mt-1 flex flex-wrap items-center gap-3 text-xs text-[var(--cn-text-muted)]">
              <span>→ <span class="font-mono text-[var(--cn-text)]">{{ updates.status.latestVersion }}</span></span>
              <span v-if="updates.status.sizeBytes">{{ formatBytes(updates.status.sizeBytes) }}</span>
              <span v-if="updates.status.requiresReboot" class="text-amber-500">{{ t('settings.about.reboot_required') }}</span>
            </div>
          </div>
        </div>
        <!-- Version stamp above the actions, and BOTH allowed to wrap.
             An earlier revision pinned this column with shrink-0 + a
             whitespace-nowrap stamp + flex-nowrap buttons. Together those made
             the card's MINIMUM width larger than the viewport, so the whole
             settings page scrolled sideways and the last button was cut off
             the screen. A control the user cannot reach is worse than one that
             wrapped onto a second line. Nothing here may refuse to shrink. -->
        <div class="flex min-w-0 flex-col items-end gap-1.5">
          <span class="font-mono text-[11px] text-[var(--cn-text-muted)]">{{ displayVersion }} · {{ buildVersion }} · {{ buildChannel }}</span>
          <div class="flex flex-wrap items-center justify-end gap-2">
          <Button v-if="!updates.isActive" size="small" severity="secondary" :loading="updates.loading" @click="onCheckNow">
            <template #icon><AppIcon name="RefreshCw" :size="13" /></template>
            <template #default>{{ t('settings.about.check_for_updates') }}</template>
          </Button>
          <Button v-if="!updates.isActive" size="small" severity="secondary" @click="openDiskUpdate">
            <template #icon><AppIcon name="HardDrive" :size="13" /></template>
            <template #default>{{ t('settings.about.load_from_disk') }}</template>
          </Button>
          <Button v-if="updates.canStart" size="small" @click="onInstall">
            <template #icon><AppIcon name="Download" :size="13" /></template>
            <template #default>{{ t('settings.about.install_now') }}</template>
          </Button>
          <Button v-if="updates.canCancel" size="small" severity="secondary" @click="onCancel">
            <template #icon><AppIcon name="X" :size="13" /></template>
            <template #default>{{ t('settings.about.cancel_update') }}</template>
          </Button>
          </div>
        </div>
      </div>

      <div v-if="updates.isActive" class="h-1.5 w-full overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
        <div class="h-full rounded-full bg-blue-500 transition-all" :style="{ width: Math.max(updates.status.progressPercent, 2) + '%' }" />
      </div>

      <div v-if="updates.lastError" class="rounded-lg border border-red-500/30 bg-red-500/10 p-3 text-xs text-red-500">{{ updates.lastError }}</div>

      <div v-if="updates.status.lastReport && !updates.isActive" class="flex flex-col gap-2 rounded-lg border p-3 text-xs" :class="updates.status.lastReport.status === 'completed' ? 'border-emerald-500/30 bg-emerald-500/5' : 'border-red-500/30 bg-red-500/10'">
        <div class="flex items-center justify-between gap-3">
          <div class="flex items-center gap-2 font-medium">
            <AppIcon :name="updates.status.lastReport.status === 'completed' ? 'CheckCircle2' : 'AlertTriangle'" :size="14" :class="updates.status.lastReport.status === 'completed' ? 'text-emerald-500' : 'text-red-500'" />
            <span class="text-[var(--cn-text)]">{{ updates.status.lastReport.from_version }} → {{ updates.status.lastReport.to_version }}</span>
            <span class="font-normal text-[var(--cn-text-muted)]">({{ updates.status.lastReport.status }})</span>
          </div>
          <Button v-if="updates.status.lastReport.backup_location" size="small" severity="secondary" :loading="rollbackInFlight" @click="onRollback">
            <template #icon><AppIcon name="Undo2" :size="13" /></template>
            <template #default>{{ t('settings.about.rollback') }}</template>
          </Button>
        </div>
        <div class="flex flex-wrap gap-x-4 gap-y-1 text-[var(--cn-text-muted)]">
          <span>{{ t('settings.about.services') }}: <span :class="updates.status.lastReport.services_ok === updates.status.lastReport.services_total ? 'text-emerald-500' : 'text-red-500'">{{ updates.status.lastReport.services_ok }}/{{ updates.status.lastReport.services_total }}</span></span>
          <span>{{ t('settings.about.gateway') }}: <span :class="updates.status.lastReport.gateway_ping ? 'text-emerald-500' : 'text-red-500'">{{ updates.status.lastReport.gateway_ping ? 'OK' : 'FAIL' }}</span></span>
          <span>{{ t('settings.about.frontend') }}: <span :class="updates.status.lastReport.frontend_ok ? 'text-emerald-500' : 'text-red-500'">{{ updates.status.lastReport.frontend_ok ? 'OK' : 'FAIL' }}</span></span>
        </div>
        <div v-if="updates.status.lastReport.services_failed?.length" class="text-red-500">{{ t('settings.about.failed_services') }}: <span class="font-mono">{{ updates.status.lastReport.services_failed.join(', ') }}</span></div>
      </div>

      <div class="flex flex-wrap items-center justify-between gap-3 border-t border-[var(--cn-border)] pt-3 text-xs">
        <label class="flex items-center gap-2 text-[var(--cn-text-muted)]"><ToggleSwitch v-model="allowReboot" /><span>{{ t('settings.about.allow_reboot') }}</span></label>
        <div class="flex gap-4 text-[var(--cn-text-muted)]">
          <span>{{ t('settings.about.last_checked') }}: <span class="text-[var(--cn-text)]">{{ formatTimestamp(updates.status.lastCheckedAt) }}</span></span>
          <span>{{ t('settings.about.last_applied') }}: <span class="text-[var(--cn-text)]">{{ formatTimestamp(updates.status.lastAppliedAt) }}</span></span>
        </div>
      </div>

      <div class="flex flex-wrap items-center justify-between gap-3">
        <div class="text-xs font-medium text-[var(--cn-text)]">{{ t('settings.about.channel') }}</div>
        <div class="inline-flex rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-0.5">
          <button v-for="c in channels" :key="c.value" type="button" :class="['rounded-md px-3 py-1 text-xs font-medium transition-colors', updates.status.channel === c.value ? 'bg-blue-500 text-white' : 'text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]']" @click="onChannelChange(c.value)">
            {{ t(`settings.about.${c.label}`) }}
          </button>
        </div>
      </div>

      <div v-if="updates.changelog.length > 0 && updates.status.updateAvailable" class="rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-3">
        <div class="mb-1 text-xs font-medium text-[var(--cn-text)]">{{ t('settings.about.changelog') }} — {{ updates.changelog[0].version }}</div>
        <pre class="whitespace-pre-wrap text-xs text-[var(--cn-text-muted)]">{{ updates.changelog[0].body }}</pre>
      </div>

      <!-- Maintenance: repair (reinstall current version's files) + clear cache -->
      <!-- Stack on narrow, single row on wide. This is the shape that cannot
           overflow: below sm the label sits above its buttons instead of
           squeezing them off the edge. -->
      <div v-if="!updates.isActive" class="flex flex-col gap-2 border-t border-[var(--cn-border)] pt-3 sm:flex-row sm:items-center sm:justify-between sm:gap-3">
        <div class="flex items-center gap-2 text-xs text-[var(--cn-text-muted)]">
          <AppIcon name="Wrench" :size="14" />
          <span>{{ t('settings.about.maintenance') }}</span>
        </div>
        <div class="flex flex-wrap items-center gap-2 sm:justify-end">
          <Button size="small" severity="secondary" :loading="repairing" @click="onRepair">
            <template #icon><AppIcon name="ShieldCheck" :size="13" /></template>
            <template #default>{{ t('settings.about.repair') }}</template>
          </Button>
          <Button v-if="hasCachedPackage" size="small" severity="secondary" :loading="clearingCache" @click="onClearCache">
            <template #icon><AppIcon name="Trash2" :size="13" /></template>
            <template #default>{{ t('settings.about.clear_cache') }}</template>
          </Button>
        </div>
      </div>
    </div>
  </SettingsCard>

  <Dialog v-model:visible="diskUpdateVisible" :header="t('settings.about.load_from_disk')" modal :style="{ width: '36rem' }" :closable="true">
    <input ref="fileInputRef" type="file" accept=".tar.gz,.tgz,.zip" class="hidden" @change="onFileSelected" />
    <div class="flex flex-col gap-3">
      <div v-if="!diskHasSearched && !diskScanning" class="flex flex-col gap-3">
        <p class="text-sm text-[var(--cn-text-muted)]">{{ t('settings.about.load_method_desc') }}</p>
        <div class="flex gap-3">
          <button class="flex flex-1 flex-col items-center gap-2 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-4 transition-colors hover:border-blue-500 hover:bg-blue-500/5" @click="openFilePicker">
            <AppIcon name="Upload" :size="24" class="text-blue-500" />
            <span class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.about.select_file') }}</span>
            <span class="text-[10px] text-[var(--cn-text-muted)]">{{ t('settings.about.select_file_hint') }}</span>
          </button>
          <button class="flex flex-1 flex-col items-center gap-2 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-4 transition-colors hover:border-blue-500 hover:bg-blue-500/5" @click="scanForPackages">
            <AppIcon name="HardDrive" :size="24" class="text-blue-500" />
            <span class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.about.scan_drives') }}</span>
            <span class="text-[10px] text-[var(--cn-text-muted)]">{{ t('settings.about.scan_drives_hint') }}</span>
          </button>
        </div>
      </div>
      <div v-if="uploadProgress" class="flex flex-col gap-2 py-4">
        <div class="flex items-baseline justify-between gap-3 text-sm">
          <span class="truncate font-medium text-[var(--cn-text)]" :title="uploadProgress.name">{{ uploadProgress.name }}</span>
          <span class="flex-shrink-0 text-xs tabular-nums text-[var(--cn-text-muted)]">{{ formatBytes(uploadProgress.loaded) }} / {{ formatBytes(uploadProgress.total) }} ({{ Math.floor((uploadProgress.loaded / Math.max(uploadProgress.total, 1)) * 100) }}%)</span>
        </div>
        <div class="h-2 overflow-hidden rounded-full bg-[var(--cn-surface-hover)]"><div class="h-full rounded-full bg-blue-500 transition-[width] duration-150" :style="{ width: `${Math.min(100, (uploadProgress.loaded / Math.max(uploadProgress.total, 1)) * 100)}%` }" /></div>
      </div>
      <div v-if="diskScanning && !uploadProgress" class="flex items-center justify-center gap-2 py-6"><AppIcon name="Loader2" :size="20" class="animate-spin text-blue-500" /><span class="text-sm text-[var(--cn-text-muted)]">{{ t('settings.about.scanning_disks') }}</span></div>
      <div v-if="diskUpdateError && !diskScanning" class="rounded-lg border border-red-500/30 bg-red-500/10 p-3 text-xs text-red-500">{{ diskUpdateError }}</div>
      <div v-if="diskHasSearched && !diskScanning && !diskUpdateError" class="flex flex-col gap-3">
        <div v-if="totalPackages === 0" class="flex flex-col items-center gap-2 py-6 text-center"><AppIcon name="HardDrive" :size="32" class="text-[var(--cn-text-muted)]" /><p class="text-sm text-[var(--cn-text-muted)]">{{ t('settings.about.no_packages_found') }}</p></div>
        <div v-for="mount in diskMounts" :key="mount.mountPoint" class="flex flex-col gap-1">
          <div class="flex items-center gap-2 text-xs font-medium text-[var(--cn-text-muted)]"><AppIcon :name="isUsbDevice(mount.device) ? 'Usb' : 'HardDrive'" :size="14" /><span class="font-mono">{{ mount.label }}</span><span class="text-[var(--cn-text-muted)]">{{ mount.device }}</span></div>
          <div v-if="mount.packages.length === 0" class="ml-5 py-1 text-xs text-[var(--cn-text-muted)] italic">{{ t('settings.about.no_packages_found') }}</div>
          <button v-for="pkg in mount.packages" :key="pkg.path" type="button" class="ml-5 flex items-center justify-between gap-3 rounded-lg border px-3 py-2 text-left transition-colors" :class="selectedPackagePath === pkg.path ? 'border-blue-500 bg-blue-500/10' : 'border-[var(--cn-border)] bg-[var(--cn-surface-hover)] hover:bg-[var(--cn-surface-hover)]'" @click="selectPackage(pkg.path)">
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2"><span class="inline-flex items-center rounded-full bg-blue-500/15 px-2 py-0.5 text-[10px] font-semibold text-blue-500">{{ pkg.version }}</span><span v-if="pkg.arch" class="text-[10px] text-[var(--cn-text-muted)]">{{ pkg.arch }}</span></div>
              <div class="mt-0.5 truncate text-xs text-[var(--cn-text-muted)]">{{ pkg.filename }}</div>
            </div>
            <div class="shrink-0 text-right text-[10px] text-[var(--cn-text-muted)]"><div>{{ formatPackageSize(pkg.sizeBytes) }}</div><div>{{ formatPackageDate(pkg.modifiedAt) }}</div></div>
          </button>
        </div>
      </div>
      <div class="flex items-center gap-2 border-t border-[var(--cn-border)] pt-3"><ToggleSwitch v-model="allowReboot" /><span class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.about.allow_reboot') }}</span></div>
    </div>
    <template #footer>
      <div class="flex justify-between">
        <div class="flex gap-2"><Button v-if="diskHasSearched" size="small" severity="secondary" :disabled="diskScanning" @click="diskHasSearched = false; diskMounts = []; selectedPackagePath = null; diskUpdateError = ''"><template #icon><AppIcon name="ArrowLeft" :size="13" /></template>{{ t('common.back') }}</Button></div>
        <div class="flex gap-2">
          <Button size="small" severity="secondary" @click="diskUpdateVisible = false">{{ t('common.cancel') }}</Button>
          <Button size="small" :disabled="!selectedPackagePath" @click="onApplyDiskUpdate"><template #icon><AppIcon name="Download" :size="13" /></template>{{ t('settings.about.apply_selected') }}</Button>
        </div>
      </div>
    </template>
  </Dialog>
</template>
