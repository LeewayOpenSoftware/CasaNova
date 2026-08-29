<script setup lang="ts">
// LiveOperationsButton — temporary topbar button that appears while
// long-running operations are in flight. Inspired by Windows 11
// taskbar tiles: a normal icon with a thin progress bar filled
// proportionally along the bottom edge. Clicking opens a popover
// listing every active operation with its own per-item progress.
//
// Sources:
//   - installProgressStore.active: compose app installs
//   - filesStore.activeOperationCount: uploads / copies / moves
//     (abstract counter — we don't have per-op names yet)
//
// When nothing is running, the button is hidden entirely. As soon
// as the first operation starts the button fades in next to the
// Files icon; when the last operation finishes it fades out.

import { ref, computed, watch, nextTick, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import OverlayPanel from 'primevue/popover'
import { useInstallProgressStore } from '@/stores/installProgress'
import { useFilesStore } from '@/stores/files'
import { useUpdatesStore } from '@/stores/updates'

const { t } = useI18n()
const installs = useInstallProgressStore()
const files = useFilesStore()
const updates = useUpdatesStore()

const popover = ref<InstanceType<typeof OverlayPanel> | null>(null)
const buttonRef = ref<HTMLButtonElement | null>(null)
let autoOpenTimer: number | null = null
onBeforeUnmount(() => { if (autoOpenTimer !== null) clearTimeout(autoOpenTimer) })

// Auto-open the popover the moment a new operation starts, so users
// get immediate feedback without having to find the button. We watch
// the total op count and only open on the 0 → ≥1 transition — adding
// a second concurrent install doesn't re-open a popover the user may
// have already dismissed.
//
// The delay before opening is important: installs kicked off from the
// InstallAppDialog fire `beginInstall()` while the dialog is still
// mid-close animation, and PrimeVue's modal mask dismissal would
// immediately re-close any popover opened inside that window. Waiting
// ~350ms lets the dialog unmount fully before we anchor the popover.
watch(
  () => installs.active.length + files.activeOperationCount + (updates.isActive ? 1 : 0),
  (count, prev) => {
    if ((prev ?? 0) !== 0 || count === 0) return
    autoOpenTimer = window.setTimeout(async () => {
      autoOpenTimer = null
      // Sanity check: the user might have already cancelled or the
      // op might have completed during the delay.
      if (installs.active.length + files.activeOperationCount + (updates.isActive ? 1 : 0) === 0) return
      // Two nextTicks so the Transition mounts the button element
      // before we try to use it as the popover anchor.
      await nextTick()
      await nextTick()
      if (buttonRef.value) {
        popover.value?.show({ currentTarget: buttonRef.value } as unknown as Event)
      }
    }, 350)
  },
)

const totalOps = computed(
  () => installs.active.length + files.activeOperationCount + (updates.isActive ? 1 : 0),
)

// Aggregate progress across all known operations. File ops don't
// report a percentage (we just know the count), so they contribute
// 50% each as a rough "in flight" signal. Installs report their
// real percentage. System updates report their real percentage from
// the UpdateManager engine. The bar shows the average across
// everything, so the more operations are running the smoother the
// progress line.
const aggregateProgress = computed(() => {
  const installProgressSum = installs.active.reduce((sum, i) => sum + i.progress, 0)
  const installCount = installs.active.length
  const fileOpsAs50 = files.activeOperationCount * 50
  const updatePart = updates.isActive ? updates.status.progressPercent : 0
  const updateCount = updates.isActive ? 1 : 0
  const total = installProgressSum + fileOpsAs50 + updatePart
  const count = installCount + files.activeOperationCount + updateCount
  if (count === 0) return 0
  return Math.round(total / count)
})

function onClick(event: MouseEvent) {
  popover.value?.toggle(event)
}
</script>

<template>
  <Transition
    enter-from-class="opacity-0 scale-90"
    enter-to-class="opacity-100 scale-100"
    leave-from-class="opacity-100 scale-100"
    leave-to-class="opacity-0 scale-90"
    enter-active-class="transition-all duration-200"
    leave-active-class="transition-all duration-150"
  >
    <button
      v-if="totalOps > 0"
      ref="buttonRef"
      type="button"
      class="relative flex h-10 w-10 items-center justify-center overflow-hidden rounded-full text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
      :aria-label="t('live_ops.title')"
      :title="t('live_ops.active', { n: totalOps }, totalOps)"
      @click="onClick"
    >
      <AppIcon name="Activity" :size="18" />
      <!-- Win11-style progress bar along the bottom edge. 2px tall,
           blue accent, width proportional to aggregateProgress. -->
      <span class="pointer-events-none absolute inset-x-1 bottom-1 h-0.5 overflow-hidden rounded-full bg-[var(--cn-border)]">
        <span
          class="block h-full rounded-full bg-blue-500 transition-all duration-300"
          :style="{ width: aggregateProgress + '%' }"
        />
      </span>
      <!-- Small count badge when 2+ ops are running. -->
      <span
        v-if="totalOps > 1"
        class="absolute -right-0.5 -top-0.5 flex h-4 min-w-[1rem] items-center justify-center rounded-full bg-blue-500 px-1 text-[9px] font-bold text-white"
      >
        {{ totalOps }}
      </span>
    </button>
  </Transition>

  <!-- Popover with per-operation progress bars. Users can see each
       install's name and percentage without leaving the page. -->
  <OverlayPanel ref="popover">
    <div class="w-80 p-3">
      <div class="mb-3 text-xs font-semibold uppercase tracking-wide text-[var(--cn-text-muted)]">
        {{ t('live_ops.title') }}
      </div>
      <div class="flex flex-col gap-3">
        <div
          v-for="install in installs.active"
          :key="install.id"
          class="flex flex-col gap-1.5"
        >
          <div class="flex items-center gap-2">
            <div class="flex h-8 w-8 shrink-0 items-center justify-center overflow-hidden rounded-md bg-[var(--cn-surface-hover)]">
              <img
                v-if="install.iconUrl"
                :src="install.iconUrl"
                :alt="install.name"
                class="h-full w-full object-contain"
              />
              <AppIcon v-else name="Package" :size="16" class="text-blue-500" />
            </div>
            <div class="min-w-0 flex-1">
              <div class="truncate text-xs font-medium text-[var(--cn-text)]">
                {{ t('live_ops.installing', { name: install.name }) }}
              </div>
              <div class="text-[10px] text-[var(--cn-text-muted)]">
                {{ Math.round(install.progress) }}%
              </div>
            </div>
          </div>
          <div class="h-1 w-full overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
            <div
              class="h-full rounded-full bg-blue-500 transition-all"
              :style="{ width: install.progress + '%' }"
            />
          </div>
        </div>

        <div
          v-if="updates.isActive"
          class="flex flex-col gap-1.5 border-t border-[var(--cn-border)] pt-3"
        >
          <div class="flex items-center gap-2">
            <div class="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-[var(--cn-surface-hover)]">
              <AppIcon name="RefreshCw" :size="16" class="text-blue-500 animate-spin" />
            </div>
            <div class="min-w-0 flex-1">
              <div class="truncate text-xs font-medium text-[var(--cn-text)]">
                {{ t(`settings.about.stage.${updates.status.stage}`) }}
                <span v-if="updates.status.latestVersion" class="text-[var(--cn-text-muted)]">
                  → {{ updates.status.latestVersion }}
                </span>
              </div>
              <div class="text-[10px] text-[var(--cn-text-muted)]">
                {{ updates.status.message || Math.round(updates.status.progressPercent) + '%' }}
              </div>
            </div>
          </div>
          <div class="h-1 w-full overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
            <div
              class="h-full rounded-full bg-blue-500 transition-all"
              :style="{ width: (updates.status.progressPercent || 5) + '%' }"
            />
          </div>
        </div>

        <div
          v-if="files.activeOperationCount > 0"
          class="flex items-center gap-2 border-t border-[var(--cn-border)] pt-3"
        >
          <div class="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-[var(--cn-surface-hover)]">
            <AppIcon name="Upload" :size="16" class="text-blue-500" />
          </div>
          <div class="min-w-0 flex-1">
            <div class="text-xs font-medium text-[var(--cn-text)]">
              {{ t('files.active_ops', { n: files.activeOperationCount }, files.activeOperationCount) }}
            </div>
            <div class="text-[10px] text-[var(--cn-text-muted)]">
              {{ t('live_ops.files_hint') }}
            </div>
          </div>
        </div>

        <div
          v-if="totalOps === 0"
          class="py-4 text-center text-xs text-[var(--cn-text-muted)]"
        >
          {{ t('live_ops.empty') }}
        </div>
      </div>
    </div>
  </OverlayPanel>
</template>
