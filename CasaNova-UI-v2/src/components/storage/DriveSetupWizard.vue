<script setup lang="ts">
import AppIcon from '@/components/ui/AppIcon.vue'
// DriveSetupWizard — a Zima-style guided flow for turning a fresh pile
// of disks into something CasaNova apps can write to. The raw
// StorageSettings page exposes every endpoint, but for users coming
// from a packaged NAS the friction of "format → mount → maybe pool"
// is overwhelming. This wizard walks four steps:
//
//   1. Welcome — explain what's about to happen
//   2. Pick disks — checkbox list of unmounted disks
//   3. Choose layout — Single disk / MergerFS pool / Cancel
//   4. Confirm + apply
//
// Layout choices map to existing backend endpoints:
//   - Single → mount one disk at the next /DATA/diskN slot
//   - Pool   → create a mergerfs over N disks, expose as one /DATA
//     entry
//
// We deliberately do NOT offer mdadm RAID in v0.1 — CasaNova uses
// mergerfs for pooling because it survives uneven disk sizes, lets
// you remove a disk without rebuilding, and degrades gracefully. We
// explain that on the layout step so users coming from QNAP/Synology
// understand why "RAID 1" isn't an option here.
//
// All long-running calls (format, mount, pool create) are wrapped in
// safeCall so a failed step surfaces a recoverable error message
// instead of leaving the wizard in a stuck state.

import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import Checkbox from 'primevue/checkbox'
import {
  listDisks,
  formatDisk,
  mountDisk,
  createMergedPool,
  type DiskEntry,
} from '@/api/storage'
import { safeCall } from '@/api/safeCall'

const props = defineProps<{
  visible: boolean
}>()
const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
  (e: 'completed'): void
}>()

const { t } = useI18n()

type Step = 'welcome' | 'pick' | 'layout' | 'apply'
const step = ref<Step>('welcome')

// ---- State -----------------------------------------------------------

const allDisks = ref<DiskEntry[]>([])
const loadingDisks = ref(false)
const errorMsg = ref<string | null>(null)
const selectedDevices = ref<string[]>([])
const layout = ref<'single' | 'pool' | null>(null)
const poolName = ref('DATA')
const applying = ref(false)
const applyLog = ref<string[]>([])

const eligibleDisks = computed(() =>
  // Unmounted, supported, non-USB by default — USB sticks make terrible
  // pool members because they detach. Users who explicitly want a USB
  // single-disk mount can fall back to the raw StorageSettings page.
  allDisks.value.filter((d) => !d.mountPoint && !d.isUSB),
)

const selectedDisks = computed(() =>
  allDisks.value.filter((d) => selectedDevices.value.includes(d.device)),
)

const totalSizeBytes = computed(() =>
  selectedDisks.value.reduce((acc, d) => acc + d.sizeBytes, 0),
)

const canAdvanceFromPick = computed(() => selectedDevices.value.length > 0)
const canAdvanceFromLayout = computed(() => {
  if (!layout.value) return false
  if (layout.value === 'pool' && selectedDevices.value.length < 2) return false
  return true
})

// ---- Step handlers ---------------------------------------------------

async function reloadDisks(): Promise<void> {
  loadingDisks.value = true
  errorMsg.value = null
  const result = await safeCall(() => listDisks())
  loadingDisks.value = false
  if (result.error) {
    errorMsg.value = result.error
    return
  }
  allDisks.value = result.data ?? []
}

function toggleDisk(device: string): void {
  const i = selectedDevices.value.indexOf(device)
  if (i >= 0) selectedDevices.value.splice(i, 1)
  else selectedDevices.value.push(device)
  // Auto-suggest a sensible layout based on count.
  if (!layout.value || (selectedDevices.value.length > 1 && layout.value === 'single')) {
    layout.value = selectedDevices.value.length > 1 ? 'pool' : 'single'
  }
}

function setStep(s: Step): void {
  step.value = s
  errorMsg.value = null
}

function back(): void {
  if (step.value === 'pick') setStep('welcome')
  else if (step.value === 'layout') setStep('pick')
  else if (step.value === 'apply') setStep('layout')
}

function next(): void {
  if (step.value === 'welcome') {
    reloadDisks()
    setStep('pick')
  } else if (step.value === 'pick' && canAdvanceFromPick.value) {
    setStep('layout')
  } else if (step.value === 'layout' && canAdvanceFromLayout.value) {
    setStep('apply')
    void applyChanges()
  }
}

function close(): void {
  // Don't close while the apply step is in flight — the user could
  // cancel mid-format and end up with a half-formatted disk. Setting
  // `applying.value` to false here would also lie about completion.
  if (applying.value) return
  emit('update:visible', false)
  // Reset on close so the next open starts fresh.
  step.value = 'welcome'
  selectedDevices.value = []
  layout.value = null
  errorMsg.value = null
  applyLog.value = []
}

// ---- Apply -----------------------------------------------------------

async function applyChanges(): Promise<void> {
  applying.value = true
  applyLog.value = []
  errorMsg.value = null

  const pushLog = (msg: string) => {
    applyLog.value.push(msg)
  }

  try {
    if (layout.value === 'single') {
      const d = selectedDisks.value[0]
      pushLog(t('settings.storage.wizard.log_formatting', { device: d.device }))
      // Default to ext4 — most home-server users have no reason to
      // pick anything else, and ext4 is what every container expects.
      // Power users can fall back to the raw StorageSettings page for
      // btrfs / xfs / zfs.
      const fmt = await safeCall(() => formatDisk(d.device, 'ext4'))
      if (fmt.error) throw new Error(fmt.error)
      pushLog(t('settings.storage.wizard.log_mounting', { device: d.device }))
      // Backend chooses the next /DATA/diskN slot when we leave the
      // mountpoint empty. Pass the device as both the device and the
      // optional friendly name so the dashboard label matches.
      const mnt = await safeCall(() => mountDisk(d.device))
      if (mnt.error) throw new Error(mnt.error)
    } else if (layout.value === 'pool') {
      pushLog(
        t('settings.storage.wizard.log_formatting_pool', {
          n: selectedDisks.value.length,
        }),
      )
      // Format each member first. We do this serially rather than
      // parallel — formatting two disks at once thrashes the bus on
      // SATA controllers and the wall-clock cost of doing them in
      // series is only a few seconds longer on typical NAS hardware.
      for (const d of selectedDisks.value) {
        pushLog(t('settings.storage.wizard.log_formatting', { device: d.device }))
        const fmt = await safeCall(() => formatDisk(d.device, 'ext4'))
        if (fmt.error) throw new Error(fmt.error)
      }
      pushLog(t('settings.storage.wizard.log_creating_pool', { name: poolName.value }))
      const pool = await safeCall(() =>
        createMergedPool({
          name: poolName.value,
          devices: selectedDevices.value,
        }),
      )
      if (pool.error) throw new Error(pool.error)
    }
    pushLog(t('settings.storage.wizard.log_done'))
    emit('completed')
  } catch (e) {
    errorMsg.value = e instanceof Error ? e.message : String(e)
  } finally {
    applying.value = false
  }
}

// ---- Format helpers --------------------------------------------------

function fmtBytes(n: number): string {
  if (n >= 1024 ** 4) return `${(n / 1024 ** 4).toFixed(1)} TB`
  if (n >= 1024 ** 3) return `${(n / 1024 ** 3).toFixed(1)} GB`
  if (n >= 1024 ** 2) return `${(n / 1024 ** 2).toFixed(0)} MB`
  return `${n} B`
}

// Reload disk list whenever the wizard opens so a user who plugged in
// a new disk between sessions sees it without restarting the dialog.
watch(
  () => props.visible,
  (v) => {
    if (v) reloadDisks()
  },
)
</script>

<template>
  <Dialog
    :visible="props.visible"
    modal
    :closable="!applying"
    :draggable="false"
    :style="{ width: '38rem' }"
    :header="t('settings.storage.wizard.header')"
    @update:visible="close"
  >
    <!-- Welcome ------------------------------------------------------- -->
    <section v-if="step === 'welcome'" class="flex flex-col gap-4">
      <div class="flex items-start gap-3">
        <AppIcon name="Sparkles" class="mt-1 shrink-0 text-blue-500" :size="22" />
        <div class="flex flex-col gap-2 text-sm">
          <p>{{ t('settings.storage.wizard.welcome_body_1') }}</p>
          <p>{{ t('settings.storage.wizard.welcome_body_2') }}</p>
        </div>
      </div>
    </section>

    <!-- Pick disks ---------------------------------------------------- -->
    <section v-else-if="step === 'pick'" class="flex flex-col gap-3">
      <p class="text-sm text-slate-600 dark:text-slate-300">
        {{ t('settings.storage.wizard.pick_instructions') }}
      </p>

      <div
        v-if="loadingDisks"
        class="rounded border border-slate-200 p-3 text-sm text-slate-500 dark:border-slate-700"
      >
        {{ t('common.loading') }}
      </div>

      <div
        v-else-if="eligibleDisks.length === 0"
        class="rounded border border-amber-300 bg-amber-50 p-3 text-sm text-amber-800 dark:border-amber-900 dark:bg-amber-950 dark:text-amber-200"
      >
        <AppIcon name="AlertCircle" class="mr-2 inline" :size="16" />
        {{ t('settings.storage.wizard.no_disks') }}
      </div>

      <ul v-else class="flex flex-col divide-y divide-slate-200 dark:divide-slate-700">
        <li
          v-for="d in eligibleDisks"
          :key="d.device"
          class="flex items-center gap-3 py-2"
        >
          <Checkbox
            :input-id="`disk-${d.device}`"
            :model-value="selectedDevices.includes(d.device)"
            binary
            @update:model-value="toggleDisk(d.device)"
          />
          <AppIcon name="HardDrive" :size="18" class="shrink-0 text-slate-400" />
          <label
            :for="`disk-${d.device}`"
            class="flex-1 cursor-pointer text-sm"
          >
            <div class="font-medium">{{ d.model || d.device }}</div>
            <div class="text-xs text-slate-500">
              {{ d.device }} · {{ fmtBytes(d.sizeBytes) }}
              <span v-if="d.rotational" class="ml-1">· HDD</span>
              <span v-else class="ml-1">· SSD</span>
            </div>
          </label>
        </li>
      </ul>
    </section>

    <!-- Choose layout ------------------------------------------------- -->
    <section v-else-if="step === 'layout'" class="flex flex-col gap-3">
      <p class="text-sm text-slate-600 dark:text-slate-300">
        {{ t('settings.storage.wizard.layout_intro') }}
      </p>

      <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
        <button
          type="button"
          class="flex flex-col items-start gap-1 rounded-lg border p-3 text-left text-sm transition-colors"
          :class="layout === 'single' ? 'border-blue-500 bg-blue-50 dark:bg-blue-950' : 'border-slate-200 dark:border-slate-700 hover:border-slate-400'"
          @click="layout = 'single'"
        >
          <AppIcon name="Disc3" :size="18" class="text-slate-500" />
          <span class="font-semibold">{{ t('settings.storage.wizard.layout_single') }}</span>
          <span class="text-xs text-slate-500">{{ t('settings.storage.wizard.layout_single_desc') }}</span>
        </button>
        <button
          type="button"
          class="flex flex-col items-start gap-1 rounded-lg border p-3 text-left text-sm transition-colors disabled:cursor-not-allowed disabled:opacity-50"
          :class="layout === 'pool' ? 'border-blue-500 bg-blue-50 dark:bg-blue-950' : 'border-slate-200 dark:border-slate-700 hover:border-slate-400'"
          :disabled="selectedDevices.length < 2"
          @click="layout = 'pool'"
        >
          <AppIcon name="Layers" :size="18" class="text-slate-500" />
          <span class="font-semibold">{{ t('settings.storage.wizard.layout_pool') }}</span>
          <span class="text-xs text-slate-500">{{ t('settings.storage.wizard.layout_pool_desc') }}</span>
        </button>
      </div>

      <div v-if="layout === 'pool'" class="flex flex-col gap-1 pt-2">
        <label class="text-xs font-medium">{{ t('settings.storage.wizard.pool_name') }}</label>
        <InputText v-model="poolName" class="text-sm" />
        <p class="text-xs text-slate-500">
          {{ t('settings.storage.wizard.pool_size_summary', { size: fmtBytes(totalSizeBytes) }) }}
        </p>
      </div>

      <p class="rounded bg-slate-50 p-2 text-xs text-slate-600 dark:bg-slate-800 dark:text-slate-300">
        <AppIcon name="AlertCircle" class="mr-1 inline" :size="12" />
        {{ t('settings.storage.wizard.no_raid_note') }}
      </p>
    </section>

    <!-- Apply --------------------------------------------------------- -->
    <section v-else-if="step === 'apply'" class="flex flex-col gap-3">
      <div
        v-if="!applying && !errorMsg"
        class="flex items-start gap-2 rounded border border-emerald-300 bg-emerald-50 p-3 text-sm text-emerald-800 dark:border-emerald-900 dark:bg-emerald-950 dark:text-emerald-200"
      >
        <AppIcon name="CheckCircle2" :size="16" class="mt-0.5" />
        <span>{{ t('settings.storage.wizard.apply_success') }}</span>
      </div>

      <div
        v-if="errorMsg"
        class="flex items-start gap-2 rounded border border-red-300 bg-red-50 p-3 text-sm text-red-800 dark:border-red-900 dark:bg-red-950 dark:text-red-200"
      >
        <AppIcon name="AlertCircle" :size="16" class="mt-0.5" />
        <span>{{ errorMsg }}</span>
      </div>

      <pre
        v-if="applyLog.length > 0"
        class="max-h-48 overflow-auto rounded bg-slate-50 p-2 text-xs leading-relaxed dark:bg-slate-800"
      ><span v-for="(line, i) in applyLog" :key="i">{{ line }}
</span></pre>
    </section>

    <!-- Footer -------------------------------------------------------- -->
    <template #footer>
      <div class="flex justify-between gap-2">
        <div>
          <Button
            v-if="step !== 'welcome' && step !== 'apply'"
            :label="t('common.back')"
            severity="secondary"
            text
            @click="back"
          />
        </div>
        <div class="flex gap-2">
          <Button
            v-if="step !== 'apply'"
            :label="t('common.cancel')"
            severity="secondary"
            text
            @click="close"
          />
          <Button
            v-if="step !== 'apply'"
            :label="step === 'layout' ? t('settings.storage.wizard.apply') : t('common.next')"
            :disabled="(step === 'pick' && !canAdvanceFromPick) || (step === 'layout' && !canAdvanceFromLayout)"
            severity="primary"
            @click="next"
          />
          <Button
            v-else
            :label="applying ? t('common.loading') : t('common.close')"
            :disabled="applying"
            severity="primary"
            @click="close"
          />
        </div>
      </div>
    </template>
  </Dialog>
</template>
