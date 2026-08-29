<script setup lang="ts">
// AppDetailPanel — the properties/about page for a library app.
// Shows icon, title, version, requirements, developer, category,
// architecture badges, description, screenshot gallery, pre-install
// tips, and Install / Custom Install / Update buttons.

import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import Button from 'primevue/button'
import type { CatalogApp } from '@/api/library'

const props = defineProps<{
  app: CatalogApp
  installing: boolean
  status: 'not_installed' | 'running' | 'stopped' | 'update'
}>()

const emit = defineEmits<{
  (e: 'back'): void
  (e: 'install', appId: string): void
  (e: 'custom-install', appId: string): void
}>()

const { t } = useI18n()

// Screenshot gallery state
const galleryIndex = ref(0)
const lightboxOpen = ref(false)

const screenshots = computed(() => props.app.screenshot_link ?? [])
const hasScreenshots = computed(() => screenshots.value.length > 0)

function prevScreenshot() {
  galleryIndex.value = (galleryIndex.value - 1 + screenshots.value.length) % screenshots.value.length
}
function nextScreenshot() {
  galleryIndex.value = (galleryIndex.value + 1) % screenshots.value.length
}

function title(): string { return props.app.title?.en_US ?? props.app.name }
function tagline(): string { return props.app.tagline?.en_US ?? '' }
function description(): string { return props.app.description?.en_US ?? '' }
function tips(): string { return props.app.tips?.before_install?.en_US ?? '' }

function formatMemory(mb: number | undefined): string {
  if (!mb) return '-'
  if (mb >= 1024) return (mb / 1024).toFixed(mb % 1024 === 0 ? 0 : 1) + ' GB'
  return mb + ' MB'
}

const appId = computed(() => props.app.store_app_id ?? props.app.name)

const installLabel = computed(() => {
  if (props.installing) return t('library.installed')
  if (props.status === 'update') return t('library.update')
  if (props.status === 'running' || props.status === 'stopped') return t('library.reinstall')
  return t('library.install')
})
</script>

<template>
  <div class="flex h-full flex-col">
    <!-- Header with back button -->
    <div class="flex items-center gap-3 border-b border-[var(--cn-border)] px-5 py-3">
      <button
        class="flex h-8 w-8 items-center justify-center rounded-lg text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
        @click="emit('back')"
      >
        <AppIcon name="ArrowLeft" :size="18" />
      </button>
      <span class="text-sm text-[var(--cn-text-muted)]">{{ t('library.title') }}</span>
      <AppIcon name="ChevronRight" :size="14" class="text-[var(--cn-text-muted)] opacity-40" />
      <span class="text-sm font-medium text-[var(--cn-text)]">{{ title() }}</span>
    </div>

    <!-- Scrollable content -->
    <div class="flex-1 overflow-y-auto">
      <!-- Hero section: icon + title + install -->
      <div class="border-b border-[var(--cn-border)] px-6 py-6">
        <div class="flex items-start gap-5">
          <img
            :src="app.icon"
            :alt="title()"
            class="h-24 w-24 shrink-0 rounded-2xl object-contain shadow-lg"
          />
          <div class="min-w-0 flex-1">
            <h2 class="text-2xl font-bold text-[var(--cn-text)]">{{ title() }}</h2>
            <p class="mt-1 text-sm text-[var(--cn-text-muted)]">{{ tagline() }}</p>

            <!-- Status badge -->
            <div v-if="status !== 'not_installed'" class="mt-2">
              <span
                :class="[
                  'inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[10px] font-bold uppercase tracking-wide text-white',
                  status === 'running' ? 'bg-emerald-500' : status === 'update' ? 'bg-amber-500' : 'bg-blue-500',
                ]"
              >
                <span
                  v-if="status === 'running'"
                  class="h-1.5 w-1.5 rounded-full bg-white animate-pulse"
                />
                {{ status === 'running' ? t('library.status_running') : status === 'update' ? t('library.update_available') : t('library.installed') }}
              </span>
            </div>

            <div class="mt-3 flex flex-wrap items-center gap-2">
              <Button
                :loading="installing"
                :severity="installing ? 'success' : status === 'update' ? 'warning' : undefined"
                @click="emit('install', appId)"
              >
                <template #icon>
                  <AppIcon v-if="installing" name="Check" :size="15" />
                  <AppIcon v-else-if="status === 'update'" name="ArrowUpCircle" :size="15" />
                  <AppIcon v-else name="Download" :size="15" />
                </template>
                <template #default>
                  {{ installLabel }}
                </template>
              </Button>
              <Button
                severity="secondary"
                outlined
                @click="emit('custom-install', appId)"
              >
                <template #icon><AppIcon name="Settings2" :size="14" /></template>
                <template #default>{{ t('library.custom_install') }}</template>
              </Button>
            </div>
          </div>
        </div>
      </div>

      <!-- Info cards row -->
      <div class="grid grid-cols-2 gap-3 border-b border-[var(--cn-border)] px-6 py-4 sm:grid-cols-4">
        <div class="rounded-lg bg-[var(--cn-surface)] p-3">
          <div class="mb-1 flex items-center gap-1.5 text-[10px] font-medium uppercase tracking-wide text-[var(--cn-text-muted)]">
            <AppIcon name="Tag" :size="11" />
            {{ t('library.version') }}
          </div>
          <div class="font-mono text-sm font-semibold text-[var(--cn-text)]">
            {{ app.version || '-' }}
          </div>
        </div>
        <div class="rounded-lg bg-[var(--cn-surface)] p-3">
          <div class="mb-1 flex items-center gap-1.5 text-[10px] font-medium uppercase tracking-wide text-[var(--cn-text-muted)]">
            <AppIcon name="HardDrive" :size="11" />
            {{ t('library.min_memory') }}
          </div>
          <div class="text-sm font-semibold text-[var(--cn-text)]">
            {{ formatMemory(app.min_memory) }}
          </div>
        </div>
        <div class="rounded-lg bg-[var(--cn-surface)] p-3">
          <div class="mb-1 flex items-center gap-1.5 text-[10px] font-medium uppercase tracking-wide text-[var(--cn-text-muted)]">
            <AppIcon name="User2" :size="11" />
            {{ t('library.developer_label') }}
          </div>
          <div class="truncate text-sm font-semibold text-[var(--cn-text)]">
            {{ app.developer }}
          </div>
        </div>
        <div class="rounded-lg bg-[var(--cn-surface)] p-3">
          <div class="mb-1 flex items-center gap-1.5 text-[10px] font-medium uppercase tracking-wide text-[var(--cn-text-muted)]">
            <AppIcon name="Layers" :size="11" />
            {{ t('library.category_label') }}
          </div>
          <div class="text-sm font-semibold text-[var(--cn-text)]">
            {{ app.category }}
          </div>
        </div>
      </div>

      <!-- Architecture badges -->
      <div class="flex items-center gap-2 border-b border-[var(--cn-border)] px-6 py-3">
        <AppIcon name="Cpu" :size="13" class="text-[var(--cn-text-muted)]" />
        <span class="text-xs text-[var(--cn-text-muted)]">{{ t('library.architectures') }}:</span>
        <span
          v-for="arch in app.architectures ?? []"
          :key="arch"
          class="rounded-full bg-blue-500/15 px-2 py-0.5 text-[10px] font-medium text-blue-500"
        >
          {{ arch }}
        </span>
      </div>

      <!-- Pre-install tips -->
      <div
        v-if="tips()"
        class="mx-6 mt-4 flex items-start gap-3 rounded-lg border border-amber-500/30 bg-amber-500/10 p-3"
      >
        <AppIcon name="AlertTriangle" :size="15" class="mt-0.5 shrink-0 text-amber-500" />
        <div class="text-xs leading-relaxed text-amber-200">{{ tips() }}</div>
      </div>

      <!-- Screenshot gallery -->
      <section v-if="hasScreenshots" class="px-6 py-4">
        <h3 class="mb-3 text-xs font-semibold uppercase tracking-wide text-[var(--cn-text-muted)]">
          {{ t('library.screenshots') }}
        </h3>
        <div class="relative">
          <div class="flex gap-3 overflow-x-auto pb-2">
            <button
              v-for="(src, i) in screenshots"
              :key="i"
              class="shrink-0 overflow-hidden rounded-lg border border-[var(--cn-border)] transition-all hover:border-blue-500/50"
              @click="galleryIndex = i; lightboxOpen = true"
            >
              <img
                :src="src"
                :alt="`${title()} screenshot ${i + 1}`"
                class="h-40 w-auto object-cover"
                loading="lazy"
              />
            </button>
          </div>
        </div>
      </section>

      <!-- Description -->
      <section class="px-6 py-4">
        <h3 class="mb-2 text-xs font-semibold uppercase tracking-wide text-[var(--cn-text-muted)]">
          {{ t('library.description') }}
        </h3>
        <p class="whitespace-pre-line text-sm leading-relaxed text-[var(--cn-text)]">
          {{ description() }}
        </p>
      </section>

      <!-- Author / metadata footer -->
      <div class="px-6 pb-6 text-xs text-[var(--cn-text-muted)]">
        {{ t('library.contributed_by') }} {{ app.author }}
      </div>
    </div>

    <!-- Lightbox -->
    <Teleport to="body">
      <Transition
        enter-from-class="opacity-0"
        enter-to-class="opacity-100"
        leave-from-class="opacity-100"
        leave-to-class="opacity-0"
        enter-active-class="transition-opacity duration-200"
        leave-active-class="transition-opacity duration-150"
      >
        <div
          v-if="lightboxOpen"
          class="fixed inset-0 z-[60] flex items-center justify-center bg-black/80"
          @click.self="lightboxOpen = false"
        >
          <button
            class="absolute right-4 top-4 flex h-10 w-10 items-center justify-center rounded-full bg-white/10 text-white hover:bg-white/20"
            @click="lightboxOpen = false"
          >
            <AppIcon name="X" :size="20" />
          </button>
          <button
            v-if="screenshots.length > 1"
            class="absolute left-4 top-1/2 flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full bg-white/10 text-white hover:bg-white/20"
            @click="prevScreenshot"
          >
            <AppIcon name="ChevronLeft" :size="20" />
          </button>
          <img
            :src="screenshots[galleryIndex]"
            :alt="`${title()} screenshot ${galleryIndex + 1}`"
            class="max-h-[85vh] max-w-[90vw] rounded-lg object-contain"
          />
          <button
            v-if="screenshots.length > 1"
            class="absolute right-4 top-1/2 flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full bg-white/10 text-white hover:bg-white/20"
            @click="nextScreenshot"
          >
            <AppIcon name="ChevronRight" :size="20" />
          </button>
          <div
            v-if="screenshots.length > 1"
            class="absolute bottom-6 left-1/2 -translate-x-1/2 rounded-full bg-black/50 px-3 py-1 text-xs text-white"
          >
            {{ galleryIndex + 1 }} / {{ screenshots.length }}
          </div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>
