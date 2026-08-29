<script setup lang="ts">
// StatusChip — the single pill used for state anywhere in the app
// (running/stopped, healthy/degraded, ok/warning/error, counts).
//
// Before this existed every view rolled its own: rounded vs rounded-full,
// text-[10px] vs text-xs, px-1.5/2/2.5, colours picked from raw Tailwind
// ramps — the same status looked different on three screens. One component,
// one shape, tokens for colour.
//
//   <StatusChip tone="success" icon="Play">running</StatusChip>
//   <StatusChip tone="neutral" size="sm">{{ count }}</StatusChip>
//   <StatusChip tone="danger" dot />          ← dot-only, no label
//
// `tone` maps to the semantic --cn-* tokens, so a theme (or the accent
// picker) moves every chip at once.

import { computed } from 'vue'
import AppIcon from '@/components/ui/AppIcon.vue'

const props = withDefaults(
  defineProps<{
    tone?: 'success' | 'warning' | 'danger' | 'info' | 'neutral'
    size?: 'sm' | 'md'
    icon?: string
    /** Render only a coloured dot (list rows where text would be noise). */
    dot?: boolean
  }>(),
  { tone: 'neutral', size: 'md', dot: false },
)

const toneClasses = computed(() => ({
  success: 'bg-[var(--cn-success-soft)] text-[var(--cn-success)]',
  warning: 'bg-[var(--cn-warning-soft)] text-[var(--cn-warning)]',
  danger: 'bg-[var(--cn-danger-soft)] text-[var(--cn-danger)]',
  info: 'bg-[var(--cn-accent-soft)] text-[var(--cn-accent)]',
  neutral: 'bg-[var(--cn-surface-hover)] text-[var(--cn-text-muted)]',
}[props.tone]))

const dotColor = computed(() => ({
  success: 'bg-[var(--cn-success)]',
  warning: 'bg-[var(--cn-warning)]',
  danger: 'bg-[var(--cn-danger)]',
  info: 'bg-[var(--cn-accent)]',
  neutral: 'bg-[var(--cn-text-muted)]',
}[props.tone]))

// One scale, two steps. Anything else was drift.
const sizeClasses = computed(() =>
  props.size === 'sm' ? 'px-1.5 py-0.5 text-[10px] gap-1' : 'px-2 py-0.5 text-xs gap-1.5',
)
const iconSize = computed(() => (props.size === 'sm' ? 11 : 13))
</script>

<template>
  <span
    v-if="dot"
    class="inline-block h-2 w-2 shrink-0 rounded-full"
    :class="dotColor"
  />
  <span
    v-else
    class="inline-flex items-center rounded-full font-medium"
    :class="[toneClasses, sizeClasses]"
  >
    <AppIcon v-if="icon" :name="icon" :size="iconSize" />
    <slot />
  </span>
</template>
