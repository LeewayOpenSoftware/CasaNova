<script setup lang="ts">
// AppLibraryPanel — full-screen slide-over panel for browsing and
// installing apps. Mounted once in AppLayout and toggled by the
// topbar Library icon.

import { ref, computed, watch, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import InputText from 'primevue/inputtext'
import Button from 'primevue/button'
import { useLibraryStore } from '@/stores/library'
import AppDetailPanel from '@/components/library/AppDetailPanel.vue'
import type { CatalogApp } from '@/api/library'

const props = defineProps<{ visible: boolean }>()
const emit = defineEmits<{
  (e: 'update:visible', v: boolean): void
  (e: 'custom-install', yaml: string): void
}>()

const { t } = useI18n()
const library = useLibraryStore()
const installing = ref<Set<string>>(new Set())
const selectedApp = ref<CatalogApp | null>(null)
let loaded = false

// Debounced search — 200ms delay
let searchTimer: number | null = null
function onSearchInput(q: string) {
  if (searchTimer) window.clearTimeout(searchTimer)
  searchTimer = window.setTimeout(() => {
    library.setSearch(q)
  }, 200)
}

// Clean up stale timer if the panel component unmounts mid-debounce.
onBeforeUnmount(() => {
  if (searchTimer) {
    window.clearTimeout(searchTimer)
    searchTimer = null
  }
})

// Load catalog on first open (lazy).
watch(
  () => props.visible,
  async (v) => {
    if (v && !loaded) {
      loaded = true
      try {
        await library.refresh()
      } catch (e) {
        console.error('[AppLibrary] refresh failed:', e)
      }
    }
  },
)

function close() {
  emit('update:visible', false)
}

function openDetail(app: CatalogApp) {
  selectedApp.value = app
}

function backToGrid() {
  selectedApp.value = null
}

function getTitle(app: CatalogApp): string {
  return app.title?.en_US ?? app.name
}

function getTagline(app: CatalogApp): string {
  return app.tagline?.en_US ?? ''
}

function getStatus(app: CatalogApp): 'not_installed' | 'running' | 'stopped' | 'update' {
  return library.getAppStatus(app)
}

function statusLabel(app: CatalogApp): string | null {
  const s = getStatus(app)
  if (s === 'running') return t('library.status_running')
  if (s === 'stopped') return t('library.installed')
  if (s === 'update') return t('library.update_available')
  return null
}

function statusColor(app: CatalogApp): string {
  const s = getStatus(app)
  if (s === 'running') return 'bg-emerald-500'
  if (s === 'stopped') return 'bg-blue-500'
  if (s === 'update') return 'bg-amber-500'
  return ''
}

async function onInstall(appId: string) {
  if (installing.value.has(appId)) return
  installing.value = new Set([...installing.value, appId])
  await library.installApp(appId)
  setTimeout(() => {
    installing.value = new Set([...installing.value].filter((id) => id !== appId))
  }, 2000)
}

async function onCustomInstall(appId: string) {
  const yaml = await library.getComposeYaml(appId)
  if (yaml) {
    emit('custom-install', yaml)
    close()
  }
}

function clearFilters() {
  library.setSearch('')
  library.setCategory(null)
}

function onRetry() {
  library.refresh()
}

const hasFilters = computed(() => library.searchQuery || library.selectedCategory)
</script>

<template>
  <Teleport to="body">
    <!-- Backdrop -->
    <Transition
      enter-from-class="opacity-0"
      enter-to-class="opacity-100"
      leave-from-class="opacity-100"
      leave-to-class="opacity-0"
      enter-active-class="transition-opacity duration-300"
      leave-active-class="transition-opacity duration-200"
    >
      <div
        v-if="visible"
        class="fixed inset-0 z-40 bg-black/50 backdrop-blur-sm"
        @click="close"
      />
    </Transition>

    <!-- Panel -->
    <Transition
      enter-from-class="translate-x-full"
      enter-to-class="translate-x-0"
      leave-from-class="translate-x-0"
      leave-to-class="translate-x-full"
      enter-active-class="transition-transform duration-300 ease-out"
      leave-active-class="transition-transform duration-200 ease-in"
    >
      <div
        v-if="visible"
        class="fixed inset-y-0 right-0 z-50 flex w-full max-w-[calc(100%-3rem)] flex-col bg-[var(--cn-bg)] shadow-2xl sm:max-w-[calc(100%-4rem)]"
        @click.stop
      >
      <!-- App detail sub-panel -->
      <AppDetailPanel
        v-if="selectedApp"
        :app="selectedApp"
        :installing="installing.has(selectedApp.name ?? selectedApp.store_app_id ?? '')"
        :status="getStatus(selectedApp)"
        @back="backToGrid"
        @install="onInstall($event)"
        @custom-install="onCustomInstall($event)"
      />

      <!-- Catalog grid -->
      <template v-else>
      <!-- Header -->
      <div class="flex items-center gap-4 border-b border-[var(--cn-border)] px-5 py-3">
        <AppIcon name="Package" :size="20" class="text-blue-500" />
        <h2 class="text-base font-semibold text-[var(--cn-text)]">
          {{ t('library.title') }}
        </h2>
        <span class="text-xs text-[var(--cn-text-muted)]">
          {{ library.totalAppCount }} {{ t('library.apps_count') }}
        </span>
        <div class="flex-1" />
        <!-- Search (debounced) -->
        <div class="relative w-64">
          <AppIcon
            name="Search"
            :size="13"
            class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-[var(--cn-text-muted)]"
          />
          <InputText
            :model-value="library.searchQuery"
            :placeholder="t('library.search')"
            class="w-full py-1.5 pl-8 text-sm"
            @update:model-value="onSearchInput($event as string)"
          />
        </div>
        <button
          class="flex h-8 w-8 items-center justify-center rounded-lg text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
          @click="close"
        >
          <AppIcon name="X" :size="18" />
        </button>
      </div>

      <div class="flex flex-1 overflow-hidden">
        <!-- Category sidebar -->
        <nav class="hidden w-44 shrink-0 overflow-y-auto border-r border-[var(--cn-border)] p-2 sm:block">
          <button
            :class="[
              'mb-0.5 flex w-full items-center gap-2 rounded-lg px-3 py-1.5 text-left text-xs font-medium transition-colors',
              !library.selectedCategory
                ? 'bg-blue-500/15 text-blue-500'
                : 'text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]',
            ]"
            @click="library.setCategory(null)"
          >
            <AppIcon name="Sparkles" :size="13" />
            {{ t('library.all_apps') }}
          </button>
          <button
            v-for="cat in library.categoryList"
            :key="cat.name"
            :class="[
              'mb-0.5 flex w-full items-center justify-between rounded-lg px-3 py-1.5 text-left text-xs transition-colors',
              library.selectedCategory === cat.name
                ? 'bg-blue-500/15 font-medium text-blue-500'
                : 'text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]',
            ]"
            @click="library.setCategory(cat.name)"
          >
            <span class="truncate">{{ cat.name }}</span>
            <span v-if="cat.count" class="shrink-0 text-[10px] opacity-40">{{ cat.count }}</span>
          </button>
        </nav>

        <!-- Main scrollable content -->
        <main class="flex-1 overflow-y-auto p-5">
          <!-- Loading spinner -->
          <div v-if="library.loading" class="flex items-center justify-center py-16">
            <AppIcon name="Loader2" :size="24" class="animate-spin text-blue-500" />
          </div>

          <!-- Error state with retry -->
          <div
            v-else-if="library.lastError && !library.fixtureMode"
            class="flex flex-col items-center justify-center py-16 text-center"
          >
            <AppIcon name="Package" :size="40" class="mb-3 text-red-500 opacity-40" />
            <div class="text-sm font-medium text-[var(--cn-text)]">{{ t('library.error_title') }}</div>
            <div class="mt-1 text-xs text-[var(--cn-text-muted)]">{{ library.lastError }}</div>
            <Button size="small" class="mt-3" @click="onRetry">
              <template #icon><AppIcon name="RefreshCw" :size="13" /></template>
              <template #default>{{ t('library.retry') }}</template>
            </Button>
          </div>

          <template v-else>
            <!-- Recommended row — only when no filters active -->
            <section v-if="!hasFilters && library.recommendedApps.length > 0" class="mb-6">
              <h3 class="mb-2 flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide text-[var(--cn-text-muted)]">
                <AppIcon name="Sparkles" :size="12" class="text-amber-500" />
                {{ t('library.recommended') }}
              </h3>
              <div class="grid grid-cols-1 gap-2 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
                <button
                  v-for="app in library.recommendedApps"
                  :key="'rec-' + app.name"
                  class="flex items-center gap-3 rounded-xl border border-[var(--cn-border)] bg-[var(--cn-surface)] p-3 text-left transition-all hover:border-blue-500/30 hover:shadow-md"
                  @click="openDetail(app)"
                >
                  <img
                    :src="app.icon"
                    :alt="getTitle(app)"
                    class="h-10 w-10 shrink-0 rounded-lg object-contain"
                    loading="lazy"
                  />
                  <div class="min-w-0 flex-1">
                    <div class="truncate text-sm font-medium text-[var(--cn-text)]">{{ getTitle(app) }}</div>
                    <div class="truncate text-[11px] text-[var(--cn-text-muted)]">{{ getTagline(app) }}</div>
                  </div>
                  <AppIcon name="Download" :size="14" class="shrink-0 text-blue-500" />
                </button>
              </div>
            </section>

            <!-- Filter chip -->
            <div v-if="hasFilters" class="mb-3 flex items-center gap-2">
              <span class="text-xs text-[var(--cn-text-muted)]">
                {{ t('library.showing', { n: library.catalogList.length }) }}
              </span>
              <button
                class="inline-flex items-center gap-1 rounded-full bg-blue-500/15 px-2 py-0.5 text-[10px] font-medium text-blue-500 hover:bg-blue-500/25"
                @click="clearFilters"
              >
                <AppIcon name="X" :size="9" />
                {{ t('library.clear_filters') }}
              </button>
            </div>

            <!-- App grid -->
            <div class="grid grid-cols-1 gap-2.5 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 2xl:grid-cols-5">
              <div
                v-for="app in library.catalogList"
                :key="app.name"
                class="group flex cursor-pointer flex-col rounded-xl border border-[var(--cn-border)] bg-[var(--cn-surface)] p-3.5 transition-all hover:border-blue-500/30 hover:shadow-md"
                @click="openDetail(app)"
              >
                <div class="mb-2 flex items-start gap-3">
                  <div class="relative shrink-0">
                    <img
                      :src="app.icon"
                      :alt="getTitle(app)"
                      class="h-12 w-12 rounded-xl object-contain"
                      loading="lazy"
                      @error="($event.target as HTMLImageElement).style.display = 'none'"
                    />
                    <!-- Status dot -->
                    <span
                      v-if="getStatus(app) !== 'not_installed'"
                      :class="['absolute -right-0.5 -top-0.5 h-3 w-3 rounded-full border-2 border-[var(--cn-surface)]', statusColor(app)]"
                    />
                  </div>
                  <div class="min-w-0 flex-1">
                    <div class="flex items-center gap-1.5">
                      <span class="truncate text-sm font-semibold text-[var(--cn-text)]">{{ getTitle(app) }}</span>
                      <!-- Status badge -->
                      <span
                        v-if="statusLabel(app)"
                        :class="[
                          'shrink-0 rounded-full px-1.5 py-0.5 text-[8px] font-bold uppercase tracking-wide text-white',
                          getStatus(app) === 'running' ? 'bg-emerald-500' : getStatus(app) === 'update' ? 'bg-amber-500' : 'bg-blue-500',
                        ]"
                      >
                        {{ statusLabel(app) }}
                      </span>
                    </div>
                    <div class="truncate text-[11px] text-[var(--cn-text-muted)]">{{ app.developer }}</div>
                    <span class="mt-0.5 inline-block rounded-full bg-[var(--cn-surface-hover)] px-1.5 py-0.5 text-[9px] text-[var(--cn-text-muted)]">
                      {{ app.category }}
                    </span>
                  </div>
                </div>
                <p class="mb-2 line-clamp-2 flex-1 text-[11px] leading-relaxed text-[var(--cn-text-muted)]">
                  {{ getTagline(app) }}
                </p>
                <div class="flex items-center justify-between">
                  <div class="flex gap-0.5">
                    <span
                      v-for="arch in (app.architectures ?? []).slice(0, 3)"
                      :key="arch"
                      class="rounded bg-[var(--cn-surface-hover)] px-1 py-0.5 text-[8px] font-medium text-[var(--cn-text-muted)]"
                    >
                      {{ arch }}
                    </span>
                  </div>
                  <Button
                    size="small"
                    :severity="installing.has(app.name) ? 'success' : getStatus(app) === 'update' ? 'warning' : undefined"
                    :disabled="installing.has(app.name)"
                    @click.stop="onInstall(app.store_app_id ?? app.name)"
                  >
                    <template #icon>
                      <AppIcon v-if="installing.has(app.name)" name="Check" :size="13" />
                      <AppIcon v-else-if="getStatus(app) === 'update'" name="ArrowUpCircle" :size="13" />
                      <AppIcon v-else name="Download" :size="13" />
                    </template>
                    <template #default>
                      {{ installing.has(app.name)
                        ? t('library.installed')
                        : getStatus(app) === 'update'
                          ? t('library.update')
                          : getStatus(app) === 'running' || getStatus(app) === 'stopped'
                            ? t('library.reinstall')
                            : t('library.install')
                      }}
                    </template>
                  </Button>
                </div>
              </div>
            </div>

            <!-- Empty state -->
            <div
              v-if="library.catalogList.length === 0 && !library.loading"
              class="flex flex-col items-center justify-center py-16 text-center"
            >
              <AppIcon name="Package" :size="40" class="mb-3 text-[var(--cn-text-muted)] opacity-40" />
              <div class="text-sm font-medium text-[var(--cn-text)]">{{ t('library.empty_title') }}</div>
              <div class="mt-1 text-xs text-[var(--cn-text-muted)]">{{ t('library.empty_subtitle') }}</div>
              <Button v-if="hasFilters" size="small" class="mt-3" @click="clearFilters">
                {{ t('library.clear_filters') }}
              </Button>
              <Button v-else size="small" class="mt-3" @click="onRetry">
                <template #icon><AppIcon name="RefreshCw" :size="13" /></template>
                <template #default>{{ t('library.retry') }}</template>
              </Button>
            </div>

            <!-- Fixture banner -->
            <div
              v-if="library.fixtureMode"
              class="mt-5 rounded-lg border border-amber-500/30 bg-amber-500/10 p-2.5 text-xs text-amber-500"
            >
              {{ t('library.fixture_warning') }}
            </div>
          </template>
        </main>
      </div>
      </template>
    </div>
  </Transition>
  </Teleport>
</template>
