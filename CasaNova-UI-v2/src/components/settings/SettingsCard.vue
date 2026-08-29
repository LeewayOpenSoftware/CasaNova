<script setup lang="ts">
// SettingsCard — a grouped section inside a settings page. Renders a
// title, optional description, optional action slot in the top-right
// corner (for things like a "Save" button aligned with the header),
// and the body slot for the actual controls.
//
// Visual: uses the default .cn-widget solid card surface, never the
// glass variant. Settings panels are functional surfaces where
// readability beats style.

// `title` is optional: a card that already says what it is through its own
// content (e.g. the CasaNova updater, which leads with a status line) can
// skip the header row entirely instead of repeating the page title.
defineProps<{
  title?: string
  description?: string
}>()
</script>

<template>
  <!-- Column + gap rather than a margin under the header: a card whose whole
       control now lives in the actions slot can have an empty body, and a
       fixed header margin billed 16px of blank space for it. `empty:hidden`
       drops the body entirely in that case (a v-if renders only a comment
       node, which still counts as :empty). -->
  <section class="cn-widget flex flex-col gap-4 p-5">
    <div v-if="title || $slots.actions" class="flex items-start justify-between gap-4">
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
    <div class="empty:hidden">
      <slot />
    </div>
  </section>
</template>
