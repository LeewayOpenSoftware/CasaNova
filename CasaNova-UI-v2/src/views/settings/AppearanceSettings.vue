<script setup lang="ts">
// Appearance settings — the visual/theming surface, split out of General
// so General stays focused on language, search, tips, modes and system
// info. Contains: light/dark mode, theme variant, accent color,
// transparency, icon pack (chrome + file/folder icons), app-icon style,
// and login/dashboard backgrounds.

import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import ToggleSwitch from 'primevue/toggleswitch'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import ThemePacksSection from '@/components/settings/ThemePacksSection.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import WallpaperPicker from '@/components/settings/WallpaperPicker.vue'
import { useThemeStore, type ThemeVariant } from '@/stores/theme'
import { useIconPackStore, type IconPackId, type AppIconStyle, type FileIconStyle } from '@/stores/iconPack'

const { t } = useI18n()
const theme = useThemeStore()
const iconPackStore = useIconPackStore()

const iconPacks: { id: IconPackId; label: string }[] = [
  { id: 'lucide', label: 'Lucide' },
  { id: 'phosphor', label: 'Phosphor' },
]

const appIconStyles: { id: AppIconStyle; labelKey: string }[] = [
  { id: 'tile', labelKey: 'settings.general.app_icon_tile' },
  { id: 'transparent', labelKey: 'settings.general.app_icon_transparent' },
]

const fileIconStyles: { id: FileIconStyle; labelKey: string }[] = [
  { id: 'line', labelKey: 'settings.general.file_icon_line' },
  { id: 'classic', labelKey: 'settings.general.file_icon_classic' },
]

// Accent swatches for the line file icons (folder yellow included so the
// classic warmth is reachable without leaving line mode).
const iconAccentPresets = ['#3b82f6', '#f5b83d', '#10b981', '#8b5cf6', '#f43f5e', '#14b8a6']

// Accent color presets. The first swatch is the default CasaOS blue so
// users always have a clear "reset" path; the custom picker covers the rest.
const accentPresets = [
  { name: 'blue', hex: '#3b82f6' },
  { name: 'emerald', hex: '#10b981' },
  { name: 'violet', hex: '#8b5cf6' },
  { name: 'rose', hex: '#f43f5e' },
  { name: 'amber', hex: '#f59e0b' },
  { name: 'teal', hex: '#14b8a6' },
]

const themeVariants: { id: ThemeVariant; label: string; description: string }[] = [
  { id: 'default', label: 'Default', description: 'Flat surfaces, modernized CasaOS look.' },
  { id: 'longhorn', label: 'Longhorn', description: 'Glossy Aero gradients, frosted-glass cards, soft glow.' },
  { id: 'zethus', label: 'Zethus', description: 'Pillowy paper cards, pill buttons, warm CasaOS lineage.' },
  { id: 'zaru', label: 'Zaru', description: 'Bright product-grade tiles, Britti Sans type, confident blue.' },
]

// Selecting Zaru also applies its signature blue as an accent preset.
function selectVariant(id: ThemeVariant) {
  theme.setThemeVariant(id)
  if (id === 'zaru') {
    theme.setAccentColor('#0057ff')
    customAccent.value = '#0057ff' // keep the custom-color picker in sync
  }
}

const customAccent = ref(theme.prefs.accentColor)
function onAccentPreset(hex: string) {
  customAccent.value = hex
  theme.setAccentColor(hex)
}
function onAccentCustom(event: Event) {
  const v = (event.target as HTMLInputElement).value
  customAccent.value = v
  theme.setAccentColor(v)
}

// Computed off the store so a backend hydrate / reset stays reflected in the
// Light/Dark buttons (a standalone ref would desync).
const isDark = computed(() => theme.prefs.darkMode)
function toggleTheme() {
  theme.setDarkMode(!theme.prefs.darkMode)
}
</script>

<template>
  <SettingsPage
    icon="Paintbrush"
    :title="t('settings.appearance.title_with_themes')"
    :description="t('settings.appearance.description')"
  >
    <SettingsCard>
      <!-- Mode subsection -->
      <div class="flex flex-col gap-2">
        <div class="text-xs font-medium text-[var(--cn-text-muted)]">
          {{ t('settings.general.mode') }}
        </div>
        <div class="flex items-center gap-3">
          <button
            :class="[
              'flex items-center gap-2 rounded-lg border px-4 py-2 text-sm transition-colors',
              isDark
                ? 'border-[var(--cn-border)] text-[var(--cn-text-muted)]'
                : 'border-blue-500 bg-blue-500/10 text-blue-500',
            ]"
            @click="isDark && toggleTheme()"
          >
            <AppIcon name="Sun" :size="16" />
            {{ t('settings.general.light') }}
          </button>
          <button
            :class="[
              'flex items-center gap-2 rounded-lg border px-4 py-2 text-sm transition-colors',
              isDark
                ? 'border-blue-500 bg-blue-500/10 text-blue-500'
                : 'border-[var(--cn-border)] text-[var(--cn-text-muted)]',
            ]"
            @click="!isDark && toggleTheme()"
          >
            <AppIcon name="Moon" :size="16" />
            {{ t('settings.general.dark') }}
          </button>
        </div>
      </div>

      <!-- Theme variant subsection -->
      <div class="mt-6 flex flex-col gap-3 border-t border-[var(--cn-border)] pt-5">
        <div class="flex items-center gap-2">
          <AppIcon name="Sparkles" :size="14" class="text-[var(--cn-text-muted)]" />
          <div class="text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.general.theme_variant') }}
          </div>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <button
            v-for="v in themeVariants"
            :key="v.id"
            type="button"
            :class="[
              'flex flex-col items-start gap-1 rounded-lg border p-3 text-left text-sm transition-colors min-w-[10rem]',
              theme.prefs.themeVariant === v.id
                ? 'border-blue-500 bg-blue-500/10 text-blue-500'
                : 'border-[var(--cn-border)] text-[var(--cn-text-muted)] hover:border-[var(--cn-text)]',
            ]"
            @click="selectVariant(v.id)"
          >
            <div class="text-sm font-semibold">{{ v.label }}</div>
            <div class="text-xs">{{ v.description }}</div>
          </button>
        </div>
        <p class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.general.theme_variant_desc') }}
        </p>
      </div>

      <!-- Accent color subsection -->
      <div class="mt-6 flex flex-col gap-3 border-t border-[var(--cn-border)] pt-5">
        <div class="flex items-center gap-2">
          <AppIcon name="Palette" :size="14" class="text-[var(--cn-text-muted)]" />
          <div class="text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.general.accent_color') }}
          </div>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <button
            v-for="preset in accentPresets"
            :key="preset.name"
            type="button"
            :title="preset.name"
            :style="{ backgroundColor: preset.hex }"
            :class="[
              'relative h-8 w-8 rounded-full border-2 transition-transform hover:scale-110',
              theme.prefs.accentColor.toLowerCase() === preset.hex.toLowerCase()
                ? 'border-[var(--cn-text)]'
                : 'border-transparent',
            ]"
            @click="onAccentPreset(preset.hex)"
          />
          <label
            class="relative flex h-8 w-8 cursor-pointer items-center justify-center overflow-hidden rounded-full border-2 border-dashed border-[var(--cn-border)] text-[var(--cn-text-muted)] hover:border-[var(--cn-text)]"
            :title="t('settings.general.accent_custom')"
          >
            <AppIcon name="Sparkles" :size="14" />
            <input
              type="color"
              :value="customAccent"
              class="absolute inset-0 h-full w-full cursor-pointer opacity-0"
              @input="onAccentCustom"
            />
          </label>
        </div>
      </div>

      <!-- Transparency subsection -->
      <div class="mt-6 flex flex-col gap-3 border-t border-[var(--cn-border)] pt-5">
        <div class="flex items-center justify-between">
          <div>
            <div class="text-sm font-medium text-[var(--cn-text)]">
              {{ t('settings.general.transparency') }}
            </div>
            <div class="mt-0.5 text-xs text-[var(--cn-text-muted)]">
              {{ t('settings.general.transparency_desc') }}
            </div>
          </div>
          <ToggleSwitch
            :model-value="theme.prefs.transparencyEnabled"
            @update:model-value="theme.setTransparency($event)"
          />
        </div>
      </div>

      <!-- Icon pack subsection — drives chrome glyphs AND file/folder icons. -->
      <div class="mt-6 flex flex-col gap-3 border-t border-[var(--cn-border)] pt-5">
        <div class="flex items-center gap-2">
          <AppIcon name="Package" :size="14" class="text-[var(--cn-text-muted)]" />
          <div class="text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.general.icon_pack') }}
          </div>
        </div>
        <div class="flex items-center gap-2">
          <button
            v-for="pack in iconPacks"
            :key="pack.id"
            :class="[
              'flex items-center gap-2 rounded-lg border px-4 py-2 text-sm transition-colors',
              iconPackStore.pack === pack.id
                ? 'border-blue-500 bg-blue-500/10 text-blue-500'
                : 'border-[var(--cn-border)] text-[var(--cn-text-muted)] hover:border-[var(--cn-text)]',
            ]"
            @click="iconPackStore.setPack(pack.id)"
          >
            <AppIcon
              :name="'Settings'"
              :size="16"
              :weight="pack.id === 'phosphor' ? 'duotone' : undefined"
            />
            {{ pack.label }}
          </button>
        </div>
        <p class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.general.icon_pack_desc') }}
        </p>
      </div>

      <!-- App icon style subsection -->
      <div class="mt-6 flex flex-col gap-3 border-t border-[var(--cn-border)] pt-5">
        <div class="flex items-center gap-2">
          <AppIcon name="LayoutGrid" :size="14" class="text-[var(--cn-text-muted)]" />
          <div class="text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.general.app_icon_style') }}
          </div>
        </div>
        <div class="flex items-center gap-2">
          <button
            v-for="s in appIconStyles"
            :key="s.id"
            :class="[
              'flex items-center gap-2 rounded-lg border px-4 py-2 text-sm transition-colors',
              iconPackStore.appIconStyle === s.id
                ? 'border-blue-500 bg-blue-500/10 text-blue-500'
                : 'border-[var(--cn-border)] text-[var(--cn-text-muted)] hover:border-[var(--cn-text)]',
            ]"
            @click="iconPackStore.setAppIconStyle(s.id)"
          >
            {{ t(s.labelKey) }}
          </button>
        </div>
        <p class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.general.app_icon_style_desc') }}
        </p>
      </div>

      <!-- Folder & file icon style subsection — Line (follows the pack)
           vs Classic (filled, traditional yellow folder). -->
      <div class="mt-6 flex flex-col gap-3 border-t border-[var(--cn-border)] pt-5">
        <div class="flex items-center gap-2">
          <AppIcon name="Folder" :size="14" class="text-[var(--cn-text-muted)]" />
          <div class="text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.general.file_icon_style') }}
          </div>
        </div>
        <div class="flex items-center gap-2">
          <button
            v-for="s in fileIconStyles"
            :key="s.id"
            :class="[
              'flex items-center gap-2 rounded-lg border px-4 py-2 text-sm transition-colors',
              iconPackStore.fileIconStyle === s.id
                ? 'border-blue-500 bg-blue-500/10 text-blue-500'
                : 'border-[var(--cn-border)] text-[var(--cn-text-muted)] hover:border-[var(--cn-text)]',
            ]"
            @click="iconPackStore.setFileIconStyle(s.id)"
          >
            {{ t(s.labelKey) }}
          </button>
        </div>
        <p class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.general.file_icon_style_desc') }}
        </p>
      </div>

      <!-- Icon accent + glow subsection — applies to LINE file icons. -->
      <div class="mt-6 flex flex-col gap-3 border-t border-[var(--cn-border)] pt-5">
        <div class="flex items-center gap-2">
          <AppIcon name="Palette" :size="14" class="text-[var(--cn-text-muted)]" />
          <div class="text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.general.icon_accent') }}
          </div>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <button
            type="button"
            :class="[
              'flex items-center gap-2 rounded-lg border px-3 py-1.5 text-sm transition-colors',
              iconPackStore.iconAccent === ''
                ? 'border-blue-500 bg-blue-500/10 text-blue-500'
                : 'border-[var(--cn-border)] text-[var(--cn-text-muted)] hover:border-[var(--cn-text)]',
            ]"
            @click="iconPackStore.setIconAccent('')"
          >
            {{ t('settings.general.icon_accent_default') }}
          </button>
          <button
            v-for="hex in iconAccentPresets"
            :key="hex"
            type="button"
            :style="{ backgroundColor: hex }"
            :class="[
              'relative h-8 w-8 rounded-full border-2 transition-transform hover:scale-110',
              iconPackStore.iconAccent.toLowerCase() === hex.toLowerCase()
                ? 'border-[var(--cn-text)]'
                : 'border-transparent',
            ]"
            :title="hex"
            @click="iconPackStore.setIconAccent(hex)"
          />
          <label
            class="relative flex h-8 w-8 cursor-pointer items-center justify-center overflow-hidden rounded-full border-2 border-dashed border-[var(--cn-border)] text-[var(--cn-text-muted)] hover:border-[var(--cn-text)]"
            :title="t('settings.general.accent_custom')"
          >
            <AppIcon name="Sparkles" :size="14" />
            <input
              type="color"
              :value="iconPackStore.iconAccent || '#3b82f6'"
              class="absolute inset-0 h-full w-full cursor-pointer opacity-0"
              @input="iconPackStore.setIconAccent(($event.target as HTMLInputElement).value)"
            />
          </label>
        </div>
        <div class="flex items-center justify-between">
          <div>
            <div class="text-sm font-medium text-[var(--cn-text)]">
              {{ t('settings.general.icon_glow') }}
            </div>
            <div class="mt-0.5 text-xs text-[var(--cn-text-muted)]">
              {{ t('settings.general.icon_glow_desc') }}
            </div>
          </div>
          <ToggleSwitch
            :model-value="iconPackStore.iconGlow"
            @update:model-value="iconPackStore.setIconGlow($event)"
          />
        </div>
        <p class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.general.icon_accent_desc') }}
        </p>
      </div>

      <!-- Login background subsection -->
      <div class="mt-6 flex flex-col gap-3 border-t border-[var(--cn-border)] pt-5">
        <div class="flex items-center gap-2">
          <AppIcon name="Image" :size="14" class="text-[var(--cn-text-muted)]" />
          <div class="text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.general.login_bg') }}
          </div>
        </div>
        <WallpaperPicker
          :model-value="theme.prefs.loginBackgroundUrl"
          @update:model-value="theme.setLoginBackground($event)"
        />
      </div>

      <!-- Dashboard background subsection -->
      <div class="mt-6 flex flex-col gap-3 border-t border-[var(--cn-border)] pt-5">
        <div class="flex items-center gap-2">
          <AppIcon name="Image" :size="14" class="text-[var(--cn-text-muted)]" />
          <div class="text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.general.dashboard_bg') }}
          </div>
        </div>
        <WallpaperPicker
          :model-value="theme.prefs.dashboardBackgroundUrl"
          @update:model-value="theme.setDashboardBackground($event)"
        />
        <label
          class="mt-2 flex cursor-pointer items-center gap-2 text-sm"
          :class="{ 'cursor-not-allowed opacity-50': !theme.prefs.dashboardBackgroundUrl }"
        >
          <input
            type="checkbox"
            :checked="theme.prefs.replicateBackground"
            :disabled="!theme.prefs.dashboardBackgroundUrl"
            class="h-4 w-4 rounded border-[var(--cn-border)] accent-blue-500"
            @change="theme.setReplicateBackground(($event.target as HTMLInputElement).checked)"
          />
          <span class="text-[var(--cn-text)]">{{ t('settings.general.replicate_bg') }}</span>
        </label>
        <p class="-mt-1 ml-6 text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.general.replicate_bg_desc') }}
        </p>
      </div>
    </SettingsCard>

    <!-- Theme packs, folded in from what used to be its own rail entry.
         Choosing a theme and managing the packs themes come from are the
         same errand; splitting them meant picking a pack on one page and
         navigating to another to apply it. The heading keeps the section
         legible as its own thing inside the merged page. -->
    <div class="mt-2 flex items-center gap-2 border-t border-[var(--cn-border)] pt-5">
      <AppIcon name="Palette" :size="15" class="text-[var(--cn-text-muted)]" />
      <div>
        <h2 class="text-sm font-semibold text-[var(--cn-text)]">
          {{ t('settings.theme_packs.title') }}
        </h2>
        <p class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.theme_packs.subtitle') }}
        </p>
      </div>
    </div>
    <ThemePacksSection />
  </SettingsPage>
</template>
