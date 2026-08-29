<script setup lang="ts">
// ShutdownConfirmDialog — confirms a hard power action before it
// fires. One component handles both shutdown and restart via the
// `mode` prop so the copy stays in sync. Real backend wiring hits
// POST /v1/sys/shutdown or POST /v1/sys/reboot and the UI navigates
// to a "goodbye" / "coming back up" screen. For the scaffold we just
// push a toast and close.

import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useInstallProgressStore } from '@/stores/installProgress'
import { systemApi } from '@/api'
import { safeCall } from '@/api/safeCall'

const props = defineProps<{
  visible: boolean
  mode: 'shutdown' | 'restart'
}>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const { t } = useI18n()
const installs = useInstallProgressStore()
const shuttingDown = ref(false)
// Keep the dialog open while the request is in flight so the user
// can see spinner/error feedback. Previously the dialog closed the
// instant the button was clicked and any backend error (auth expiry,
// systemd failure, network blip) showed up as a rootless toast with
// no way to retry in context.
const submitting = ref(false)

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

// Mode-driven copy + icon. The header pulls from the same map so the
// dialog title, body, and confirm button label all stay consistent.
const modeMeta = computed(() => {
  if (props.mode === 'shutdown') {
    return {
      title: t('topbar.power.shutdown'),
      body: t('topbar.power.shutdown_confirm'),
      confirm: t('topbar.power.shutdown_confirm_btn'),
      icon: 'Power',
      tone: 'text-red-500',
    }
  }
  return {
    title: t('topbar.power.restart'),
    body: t('topbar.power.restart_confirm'),
    confirm: t('topbar.power.restart_confirm_btn'),
    icon: 'RotateCw',
    tone: 'text-amber-500',
  }
})

async function onConfirm() {
  if (submitting.value) return
  submitting.value = true
  // Real backend wiring — calls PUT /v1/sys/state/{restart|off}.
  const result = await safeCall(
    () => (props.mode === 'shutdown' ? systemApi.shutdown() : systemApi.reboot()),
  )
  submitting.value = false
  if (result.error) {
    // Leave the dialog open so the user can retry or cancel with
    // full context — the toast is secondary feedback.
    installs.pushToast('error', result.error)
    return
  }
  dialogVisible.value = false
  installs.pushToast(
    'info',
    props.mode === 'shutdown'
      ? t('topbar.power.shutdown_toast')
      : t('topbar.power.restart_toast'),
  )
  // Transition to a fullscreen overlay indicating the system is going down.
  setTimeout(() => {
    shuttingDown.value = true
  }, 1500)
}
</script>

<template>
  <!-- Fullscreen overlay shown after the power action succeeds -->
  <Teleport to="body">
    <div
      v-if="shuttingDown"
      class="fixed inset-0 z-[9999] flex flex-col items-center justify-center bg-black/95 text-white"
    >
      <AppIcon :name="modeMeta.icon" :size="48" class="mb-4 animate-pulse" :class="modeMeta.tone" />
      <p class="text-lg font-semibold">
        {{ mode === 'shutdown' ? t('topbar.power.shutting_down') : t('topbar.power.restarting') }}
      </p>
    </div>
  </Teleport>

  <Dialog
    v-model:visible="dialogVisible"
    modal
    :style="{ width: '24rem' }"
    :header="modeMeta.title"
    :dismissable-mask="true"
  >
    <div class="flex items-start gap-3">
      <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-[var(--cn-surface-hover)]">
        <AppIcon :name="modeMeta.icon" :size="18" :class="modeMeta.tone" />
      </div>
      <div class="flex-1 text-sm text-[var(--cn-text)]">
        {{ modeMeta.body }}
      </div>
    </div>
    <div class="mt-3 flex items-start gap-2 rounded-lg border border-amber-500/30 bg-amber-500/10 p-3">
      <AppIcon name="AlertTriangle" :size="14" class="mt-0.5 shrink-0 text-amber-500" />
      <div class="text-xs text-[var(--cn-text-muted)]">
        {{ t('topbar.power.warning') }}
      </div>
    </div>

    <template #footer>
      <Button
        :label="t('common.cancel')"
        severity="secondary"
        :disabled="submitting"
        @click="dialogVisible = false"
      />
      <Button
        :label="modeMeta.confirm"
        :severity="mode === 'shutdown' ? 'danger' : 'warning'"
        :loading="submitting"
        :disabled="submitting"
        @click="onConfirm"
      />
    </template>
  </Dialog>
</template>
