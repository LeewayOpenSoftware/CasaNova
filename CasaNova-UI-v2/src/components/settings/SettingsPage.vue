<script setup lang="ts">
// SettingsPage — the shared shell every settings domain page renders
// inside. Handles the common layout bits (icon + title + description
// header, scroll container, consistent vertical rhythm) so each domain
// page only has to produce its actual content.
//
// Usage from a domain page:
//   <SettingsPage icon="HardDrive" title="Storage" description="...">
//     <SettingsCard title="Mount points">...</SettingsCard>
//   </SettingsPage>
//
// `icon` takes an icon NAME (string) and renders it through AppIcon, so the
// page follows the user's icon pack. It still accepts a raw Component for
// the odd caller that imports a glyph the registry doesn't carry.

import type { Component } from 'vue'
import AppIcon from '@/components/ui/AppIcon.vue'

defineProps<{
  icon?: Component | string
  iconSrc?: string
  title: string
  description?: string
}>()
</script>

<template>
  <div class="flex flex-col gap-6">
    <!-- Page header: icon + title + optional one-line description.
         Matches the rhythm we'll see on every settings page so users
         always know where they are. -->
    <header class="flex flex-wrap items-start gap-4 sm:flex-nowrap">
      <div
        class="flex h-12 w-12 flex-shrink-0 items-center justify-center overflow-hidden rounded-xl border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] text-blue-500"
      >
        <img v-if="iconSrc" :src="iconSrc" alt="" class="h-full w-full object-contain" />
        <AppIcon v-else-if="typeof icon === 'string'" :name="icon" :size="24" />
        <component v-else-if="icon" :is="icon" :size="24" />
      </div>
      <div class="min-w-0 flex-1 pt-0.5">
        <h1 class="cn-page-title text-xl font-semibold text-[var(--cn-text)]">{{ title }}</h1>
        <p v-if="description" class="cn-page-title mt-1 text-sm text-[var(--cn-text-muted)]">
          {{ description }}
        </p>
      </div>
      <!-- Trailing actions slot — optional right-side area for things
           like "Add user" buttons and live counts. Rendering it here
           keeps every settings page's actions anchored at the same
           baseline as the title, so pages with actions feel as
           harmonious as pages without them. -->
      <!-- Below sm the actions take their own line under the title
           (pl-16 = icon 48px + gap 16px): beside it they squeezed the
           title and description into a column a word wide. -->
      <div v-if="$slots.actions" class="flex shrink-0 items-center gap-3 pt-0.5 max-sm:basis-full max-sm:pl-16">
        <slot name="actions" />
      </div>
    </header>

    <!-- Body slot: a flex column with gap so settings cards stack
         with consistent spacing regardless of which page we're on. -->
    <div class="flex flex-col gap-4">
      <slot />
    </div>
  </div>
</template>
