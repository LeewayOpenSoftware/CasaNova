<script setup lang="ts">
// QuickDiskPickerDialog — slim in-place popup for attaching a new
// local storage device to CasaNova without routing away to the full
// Settings → Storage page. User clicks "Nuevo almacenamiento local"
// in the file manager sidebar's + menu, this dialog pops up, lists
// every connected disk that isn't yet mounted (plus a "deep dive"
// shortcut to the full settings page for advanced operations), and
// lets them mount/format without leaving the file manager.
//
// Reads from the existing storage fixture so the list is the same
// data the Settings → Storage page shows. Mount + Format actions
// push toasts for feedback — real backend calls are the swap point.

import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useInstallProgressStore } from '@/stores/installProgress'
import * as storageApi from '@/api/storage'
import { safeCall } from '@/api/safeCall'
import { onMounted } from 'vue'

type SmartHealth = 'good' | 'warning' | 'critical' | 'unknown'
interface DiskEntry {
  device: string
  model: string
  sizeBytes: number
  mountPoint: string | null
  isUSB: boolean
  smartHealth: SmartHealth
}

const props = defineProps<{ visible: boolean }>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const { t } = useI18n()
const router = useRouter()
const installs = useInstallProgressStore()

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

// Fetch real disks from the storage API.
const allDisks = ref<DiskEntry[]>([])

onMounted(async () => {
  const result = await safeCall(
    async () => {
      const disks = await storageApi.listDisks()
      return disks.map((d) => ({
        device: d.device,
        model: d.model,
        sizeBytes: d.sizeBytes,
        mountPoint: d.mountPoint,
        isUSB: d.isUSB,
        smartHealth: (d.smartHealth || 'unknown') as SmartHealth,
      }))
    },
    () => [] as DiskEntry[],
  )
  allDisks.value = result.data as DiskEntry[]
})

const unmountedDisks = computed(() =>
  allDisks.value.filter((d) => !d.mountPoint),
)
const mountedCount = computed(
  () => allDisks.value.filter((d) => d.mountPoint).length,
)

const healthVisuals: Record<SmartHealth, { icon: string; classes: string }> = {
  good: { icon: 'CircleCheck', classes: 'text-emerald-500' },
  warning: { icon: 'CircleAlert', classes: 'text-amber-500' },
  critical: { icon: 'CircleAlert', classes: 'text-red-500' },
  unknown: { icon: 'CircleHelp', classes: 'text-slate-400' },
}

function formatBytes(bytes: number): string {
  const tb = bytes / 1024 ** 4
  if (tb >= 1) return tb.toFixed(2) + ' TB'
  const gb = bytes / 1024 ** 3
  return gb.toFixed(2) + ' GB'
}

// ----- Actions -----
// Mount: triggers the backend mount call (POST /v1/storage) and
// pushes a toast on success.
const mounting = ref(false)
async function mount(disk: DiskEntry) {
  mounting.value = true
  try {
    await storageApi.mountDisk(disk.device)
    installs.pushToast('success', t('files.disk_picker.mount_toast', { device: disk.device }))
    dialogVisible.value = false
  } catch {
    installs.pushToast('error', t('files.disk_picker.mount_error', { device: disk.device }))
  } finally {
    mounting.value = false
  }
}

// Advanced — jump to the full Storage settings page for operations
// (format, benchmark, merge, resize) that don't fit the slim dialog.
function openSettings() {
  dialogVisible.value = false
  router.push('/app/settings/storage')
}
</script>

<template>
  <Dialog
    v-model:visible="dialogVisible"
    modal
    :style="{ width: '26rem' }"
    :dismissable-mask="true"
    :header="t('files.disk_picker.title')"
  >
    <p class="mb-4 text-xs text-[var(--cn-text-muted)]">
      {{ t('files.disk_picker.description') }}
    </p>

    <!-- Available-disk list. Each row: icon + model + device + capacity
         + SMART badge + Mount button. -->
    <div class="flex flex-col gap-2">
      <div
        v-for="disk in unmountedDisks"
        :key="disk.device"
        class="flex items-start justify-between gap-3 rounded-lg border border-[var(--cn-border)] p-3"
      >
        <div class="flex min-w-0 flex-1 items-start gap-3">
          <div
            class="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg"
            :class="disk.isUSB ? 'bg-amber-500/10 text-amber-500' : 'bg-blue-500/10 text-blue-500'"
          >
            <AppIcon :name="disk.isUSB ? 'Usb' : 'HardDrive'" :size="18" />
          </div>
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-1.5">
              <span class="truncate text-sm font-medium text-[var(--cn-text)]">
                {{ disk.model }}
              </span>
              <component
                :is="healthVisuals[disk.smartHealth].icon"
                :size="12"
                :class="healthVisuals[disk.smartHealth].classes"
              />
            </div>
            <div class="mt-0.5 font-mono text-[10px] text-[var(--cn-text-muted)]">
              {{ disk.device }} · {{ formatBytes(disk.sizeBytes) }}
            </div>
          </div>
        </div>
        <Button
          :label="t('files.disk_picker.mount')"
          size="small"
          :loading="mounting"
          @click="mount(disk)"
        />
      </div>

      <div
        v-if="unmountedDisks.length === 0"
        class="rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-4 text-center text-xs text-[var(--cn-text-muted)]"
      >
        {{ t('files.disk_picker.empty') }}
      </div>
    </div>

    <!-- Advanced shortcut row. Jumps to Settings → Storage for format,
         benchmark, merge, and resize — operations that need the full
         page rather than a slim dialog. -->
    <button
      type="button"
      class="mt-4 flex w-full items-center justify-between rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-3 py-2 text-xs text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-border)] hover:text-[var(--cn-text)]"
      @click="openSettings"
    >
      <span>
        {{ t('files.disk_picker.advanced', { n: mountedCount }, mountedCount) }}
      </span>
      <AppIcon name="ExternalLink" :size="12" />
    </button>
  </Dialog>
</template>
