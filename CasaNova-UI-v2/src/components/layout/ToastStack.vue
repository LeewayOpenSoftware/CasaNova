<script setup lang="ts">
// ToastStack — bottom-left stack of transient feedback toasts. Reads
// from the installProgress store's `toasts` array (shared so every
// component can push via a single store call). Each toast auto-
// dismisses after 4s via the store itself; this component only
// handles rendering + the click-to-dismiss affordance.
//
// Kept intentionally simple: no animation library, just Vue's
// built-in <Transition> for the enter/leave fade.

import AppIcon from '@/components/ui/AppIcon.vue'
import { useInstallProgressStore } from '@/stores/installProgress'

const installs = useInstallProgressStore()

const kindVisuals = {
  success: { icon: 'CircleCheck', classes: 'text-emerald-500' },
  error: { icon: 'CircleAlert', classes: 'text-red-500' },
  info: { icon: 'Info', classes: 'text-blue-500' },
} as const
</script>

<template>
  <div class="pointer-events-none fixed bottom-4 left-4 z-50 flex flex-col gap-2">
    <TransitionGroup
      enter-from-class="opacity-0 translate-y-4"
      enter-to-class="opacity-100 translate-y-0"
      leave-from-class="opacity-100"
      leave-to-class="opacity-0"
      enter-active-class="transition-all duration-200"
      leave-active-class="transition-opacity duration-200"
    >
      <div
        v-for="toast in installs.toasts"
        :key="toast.id"
        class="pointer-events-auto flex min-w-[260px] max-w-md items-center gap-3 rounded-xl border border-[var(--cn-border)] bg-[var(--cn-surface)] p-3 pr-2 shadow-lg"
      >
        <component
          :is="kindVisuals[toast.kind].icon"
          :size="18"
          :class="kindVisuals[toast.kind].classes"
        />
        <span class="flex-1 text-sm text-[var(--cn-text)]">{{ toast.text }}</span>
        <button
          type="button"
          class="flex h-6 w-6 items-center justify-center rounded-md text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
          aria-label="Dismiss toast notification"
          @click="installs.dismissToast(toast.id)"
        >
          <AppIcon name="X" :size="13" />
        </button>
      </div>
    </TransitionGroup>
  </div>
</template>
