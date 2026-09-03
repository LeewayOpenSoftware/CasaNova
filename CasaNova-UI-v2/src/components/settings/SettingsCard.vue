<script setup lang="ts">
// SettingsCard — a grouped section inside a settings page. Renders a
// title, optional description, optional action slot in the top-right
// corner (for things like a "Save" button aligned with the header),
// and the body slot for the actual controls.
//
// Visual: uses the default .cn-widget solid card surface, never the
// glass variant. Settings panels are functional surfaces where
// readability beats style.

import { useSlots, computed, Comment, Text, type VNode } from 'vue'

// `title` is optional: a card that already says what it is through its own
// content (e.g. the CasaNova updater, which leads with a status line) can
// skip the header row entirely instead of repeating the page title.
defineProps<{
  title?: string
  description?: string
}>()

const slots = useSlots()

function isVNodeEmpty(node: VNode | null | undefined): boolean {
  if (!node) return true
  if (node.type === Comment) return true
  if (node.type === Text && typeof node.children === 'string' && !node.children.trim()) return true
  if (Array.isArray(node.children)) {
    return (node.children as VNode[]).every(isVNodeEmpty)
  }
  return false
}

const hasBody = computed(() => {
  if (!slots.default) return false
  const nodes = slots.default()
  return nodes.some((node) => !isVNodeEmpty(node))
})
</script>

<template>
  <!-- Single-row option cards (where the control lives in the actions slot
       and the body is empty) size tightly to their content with no wasted
       gap or empty block. Multi-content cards keep gap-4 between header and body. -->
  <section class="cn-widget flex flex-col p-5" :class="{ 'gap-4': hasBody }">
    <div v-if="title || $slots.actions" class="flex items-center justify-between gap-4">
      <div v-if="title" class="min-w-0 flex-1">
        <h2 class="text-sm font-semibold text-[var(--cn-text)]">{{ title }}</h2>
        <p v-if="description" class="mt-0.5 text-xs text-[var(--cn-text-muted)]">
          {{ description }}
        </p>
      </div>
      <div v-if="$slots.actions" class="flex-shrink-0">
        <slot name="actions" />
      </div>
    </div>
    <div v-if="hasBody">
      <slot />
    </div>
  </section>
</template>
