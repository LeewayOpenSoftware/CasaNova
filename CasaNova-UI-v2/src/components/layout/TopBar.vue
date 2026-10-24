<script setup lang="ts">
// TopBar — persistent navigation chrome visible on every authenticated page.
//
// Contents (left-to-right):
//   1. Brand mark (clicking returns to home)
//   2. Flex spacer
//   3. Files quick-access icon (opens file manager route — not yet built)
//   4. Terminal quick-access icon (opens terminal modal — not yet built)
//   5. Notification bell with unread badge (toggles the slide-in sidebar)
//   6. User menu (avatar + name, dropdown with Settings + Logout)
//
// The layout is intentionally sparse in Stage 2 — placeholders for Files and
// Terminal forward to routes that don't exist yet, so the clicks no-op
// gracefully until those views are built in later stages. We log a debug
// message in dev so we know the handler fires.

import { computed, ref, nextTick } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import Button from 'primevue/button'
import Menu from 'primevue/menu'
import Avatar from 'primevue/avatar'
import Badge from 'primevue/badge'
import { useAuthStore } from '@/stores/auth'
import { useUserPrefs } from '@/composables/useUserPrefs'
import { useNotificationsStore } from '@/stores/notifications'
import { useFilesStore } from '@/stores/files'
import { useUpdatesStore } from '@/stores/updates'
import TerminalDialog from './TerminalDialog.vue'
import GlobalSearch from './GlobalSearch.vue'
import LiveOperationsButton from './LiveOperationsButton.vue'
import AppLibraryPanel from './AppLibraryPanel.vue'
import UserAccountDialog from './UserAccountDialog.vue'
import ShutdownConfirmDialog from './ShutdownConfirmDialog.vue'
import InstallAppDialog from '@/components/dashboard/InstallAppDialog.vue'
import { useLibraryStore } from '@/stores/library'

const terminalVisible = ref(false)
const accountVisible = ref(false)
const shutdownDialogVisible = ref(false)
const shutdownMode = ref<'shutdown' | 'restart'>('shutdown')
const libraryStore = useLibraryStore()
const customInstallDialogVisible = ref(false)
const customInstallYaml = ref('')

function onCustomInstallFromLibrary(yaml: string) {
  customInstallYaml.value = yaml
  customInstallDialogVisible.value = true
}
// Files store exposes an activeOperationCount that ticks while uploads
// or copies are in flight. We read it here to render a small dot on
// the Files icon so users who minimize the file manager mid-upload
// see their operations are still alive.
const filesStore = useFilesStore()
const updatesStore = useUpdatesStore()

const router = useRouter()
const { t } = useI18n()
const auth = useAuthStore()

// ── Editable / removable branding name (top-left wordmark) ──────────
const { brandName } = useUserPrefs()
const DEFAULT_BRAND_NAME = 'CasaNova'
// null/unset -> default; '' -> hidden (icon-only); else the custom name.
const brandDisplayName = computed(() => brandName.value ?? DEFAULT_BRAND_NAME)
const brandNameHidden = computed(() => brandName.value === '')
// Only admins get the rename affordance. (The pref is per-user, so this
// simply lets each admin personalize their own top-left copy.)
const canEditBrand = computed(() => auth.user?.role === 'admin')

const brandEditing = ref(false)
const brandDraft = ref('')
const brandInput = ref<HTMLInputElement | null>(null)

function enterBrandEdit() {
  brandDraft.value = brandDisplayName.value
  brandEditing.value = true
  nextTick(() => brandInput.value?.focus())
}
function cancelBrandEdit() {
  brandEditing.value = false
}
function saveBrandName() {
  const next = brandDraft.value.trim()
  // Empty field on save == "hide the name" (persist '' so it survives reload).
  brandName.value = next === '' ? '' : next
  brandEditing.value = false
}
function hideBrandName() {
  // Explicit icon-only: clears the visible name, keeps the mark.
  brandName.value = ''
  brandEditing.value = false
}
const notifications = useNotificationsStore()

// Derived from the user's nickname / username for the avatar fallback.
// User menu shortcuts — the common set a home-server user reaches
// for from the avatar: account editor (name/password/avatar), about
// dialog, hard power actions (shutdown/restart), and log out. Each
// entry carries a `kind` string so the slot template can swap in the
// right Lucide icon at render time.
const userMenu = ref()
const userMenuItems = computed(() => [
  {
    label: t('account.title'),
    kind: 'account',
    command: () => (accountVisible.value = true),
  },
  {
    label: t('settings.about.title'),
    kind: 'about',
    command: () => router.push('/app/settings/about'),
  },
  { separator: true },
  {
    label: t('topbar.power.restart'),
    kind: 'restart',
    command: () => {
      shutdownMode.value = 'restart'
      shutdownDialogVisible.value = true
    },
  },
  {
    label: t('topbar.power.shutdown'),
    kind: 'shutdown',
    command: () => {
      shutdownMode.value = 'shutdown'
      shutdownDialogVisible.value = true
    },
  },
  { separator: true },
  {
    label: t('common.logout'),
    kind: 'logout',
    command: () => {
      auth.logout()
      router.push({ name: 'login' })
    },
  },
])

function openUserMenu(event: Event) {
  userMenu.value?.toggle(event)
}

function goHome() {
  router.push('/app')
}

function openFiles() {
  // Routes to the file manager section. The parent route redirects
  // into /app/files which renders the browser at /DATA by default.
  router.push('/app/files')
}

function openApps() {
  // The internal browser for the box's apps. One click from anywhere, same
  // rank as Files and Notes: it is a place you go, not a setting you find.
  router.push('/app/apps')
}

function openNotes() {
  // Notes sits beside Files in the icon row rather than inside it. The feature
  // is "read the command I left on the other machine", and that is worth one
  // click from anywhere, not two plus a sidebar.
  router.push('/app/notes')
}

const globalSearchOpen = ref(false)

function openTerminal() {
  terminalVisible.value = true
}

function openSettings() {
  // Settings has a dedicated top-bar icon — matching the CasaOS icon-row
  // pattern but as a proper routed section (not a dropdown flyout).
  router.push('/app/settings')
}

function toggleNotifications() {
  notifications.toggleSidebar()
}

// Below `sm` (640px) the full icon row does not fit: on a 390px phone it
// pushed the page to ~677px and every route scrolled sideways. The icons
// that are places to go (Files, Notes, Apps, Library, Terminal, Settings)
// fold into this menu there; Search, live operations, notifications and
// the avatar stay in the bar. At `sm` and up the menu button is hidden and
// the row is exactly what it was.
const moreMenu = ref()
const moreMenuItems = computed(() => [
  { label: t('topbar.files'), iconName: 'Files', command: openFiles },
  { label: t('topbar.notes'), iconName: 'NotebookPen', command: openNotes },
  { label: t('topbar.apps'), iconName: 'AppWindow', command: openApps },
  { label: t('library.title'), iconName: 'LayoutGrid', command: () => libraryStore.openPanel() },
  { label: t('topbar.terminal'), iconName: 'Terminal', command: openTerminal },
  { label: t('topbar.settings'), iconName: 'Settings', command: openSettings },
])
// The two activity dots of the folded icons (file operations, update
// available) must not vanish with them.
const moreMenuHasActivity = computed(
  () => filesStore.activeOperationCount > 0
    || (updatesStore.status.updateAvailable && !updatesStore.isActive),
)

function openMoreMenu(event: Event) {
  moreMenu.value?.toggle(event)
}
</script>

<template>
  <header
    class="cn-topbar flex h-14 items-center gap-1 border-b border-slate-200 bg-[var(--cn-surface)] px-3 dark:border-slate-800 sm:gap-3 sm:px-4"
  >
    <!-- Brand: CasaNova wordmark with the "orb with clouds" mark from
         public/assets/brand/casanova2.png. This is the "core / center"
         glyph — the same orb the login avatar (casanova.png) decorates
         a house with, but stripped down to the orb itself so it reads
         as a platform mark rather than a product illustration. -->
    <!-- Brand group: icon + editable/removable name. Admins get a hover
         pencil to rename the wordmark or hide it (icon-only). The name is
         a per-user pref (casanova.prefs.brandName) synced via the existing
         ui-prefs endpoint — no backend change. Pencil/editor are SIBLINGS
         of the home button (never nested — nested <button> is invalid). -->
    <div class="group flex min-w-0 items-center gap-1">
      <button
        class="flex min-w-0 items-center gap-2 text-lg font-bold text-slate-900 dark:text-slate-100"
        @click="goHome"
      >
        <span class="relative flex h-8 w-8 shrink-0 items-center justify-center">
          <img
            src="/assets/brand/casanova2.png"
            alt=""
            class="absolute h-10 w-10 object-contain"
            aria-hidden="true"
          />
        </span>
        <span v-if="!brandEditing && !brandNameHidden" class="max-sm:truncate">{{ brandDisplayName }}</span>
      </button>

      <!-- Pencil: admin-only, fades in on hover of the brand group. -->
      <button
        v-if="canEditBrand && !brandEditing"
        type="button"
        class="flex h-6 w-6 shrink-0 items-center justify-center rounded-md text-[var(--cn-text-muted)] opacity-0 transition-opacity hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)] focus:opacity-100 group-hover:opacity-100"
        :aria-label="t('topbar.brand.edit')"
        :title="t('topbar.brand.edit')"
        @click="enterBrandEdit"
      >
        <AppIcon name="Pencil" :size="13" />
      </button>

      <!-- Inline editor: text field + save / cancel + hide (icon-only). -->
      <div v-if="brandEditing" class="flex items-center gap-1">
        <input
          ref="brandInput"
          v-model="brandDraft"
          type="text"
          maxlength="32"
          class="w-40 rounded-md border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-2 py-1 text-sm font-semibold text-[var(--cn-text)] focus:outline-none focus:ring-2 focus:ring-blue-500/40"
          :placeholder="t('topbar.brand.placeholder')"
          @keyup.enter="saveBrandName"
          @keyup.escape="cancelBrandEdit"
        />
        <button
          type="button"
          class="flex h-6 w-6 shrink-0 items-center justify-center rounded-md text-emerald-500 hover:bg-[var(--cn-surface-hover)]"
          :aria-label="t('common.save')"
          :title="t('common.save')"
          @click="saveBrandName"
        >
          <AppIcon name="Check" :size="14" />
        </button>
        <button
          type="button"
          class="flex h-6 w-6 shrink-0 items-center justify-center rounded-md text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
          :aria-label="t('common.cancel')"
          :title="t('common.cancel')"
          @click="cancelBrandEdit"
        >
          <AppIcon name="X" :size="14" />
        </button>
        <button
          type="button"
          class="flex h-6 shrink-0 items-center gap-1 rounded-md px-2 text-xs text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
          :aria-label="t('topbar.brand.hide')"
          :title="t('topbar.brand.hide')"
          @click="hideBrandName"
        >
          <AppIcon name="EyeOff" :size="14" />
          <span>{{ t('topbar.brand.hide') }}</span>
        </button>
      </div>
    </div>

    <div class="flex-1" />

    <!-- Quick-access icons. Each is a square icon button so they read as
         a group on both desktop and mobile. `severity="secondary"` uses
         the Aura preset's subtle slate tone instead of the primary green. -->
    <!-- Files icon with an activity dot that lights up while the
         files store has in-flight operations (uploads, copies, moves).
         Users can minimize the file manager mid-upload and still see
         at a glance that their work is progressing. -->
    <div class="relative hidden sm:block">
      <Button
        text
        rounded
        severity="secondary"
        :aria-label="t('topbar.files')"
        :title="filesStore.activeOperationCount > 0
          ? t('files.active_ops', { n: filesStore.activeOperationCount }, filesStore.activeOperationCount)
          : t('topbar.files')"
        @click="openFiles"
      >
        <AppIcon name="Files" :size="20" />
      </Button>
      <span
        v-if="filesStore.activeOperationCount > 0"
        class="pointer-events-none absolute right-1 top-1 flex h-2.5 w-2.5"
      >
        <span class="absolute inline-flex h-full w-full animate-ping rounded-full bg-blue-500 opacity-75" />
        <span class="relative inline-flex h-2.5 w-2.5 rounded-full bg-blue-500" />
      </span>
    </div>

    <!-- Notes: server-side scratchpad. Sits next to Files because it is the
         same kind of thing — content that lives on the box — and because its
         whole value is being one click away. -->
    <div class="hidden sm:contents">
      <Button
        text
        rounded
        severity="secondary"
        :aria-label="t('topbar.notes')"
        :title="t('topbar.notes')"
        @click="openNotes"
      >
        <AppIcon name="NotebookPen" :size="20" />
      </Button>
    </div>

    <!-- Apps: the internal browser. Embedded panels for apps that consent
         to framing; honest new-tab fallbacks for the ones that refuse. -->
    <div class="hidden sm:contents">
      <Button
        text
        rounded
        severity="secondary"
        :aria-label="t('topbar.apps')"
        :title="t('topbar.apps')"
        @click="openApps"
      >
        <AppIcon name="AppWindow" :size="20" />
      </Button>
    </div>

    <!-- App Library button -->
    <!-- Global file search (casanova-search) across all storage. -->
    <Button
      text
      rounded
      severity="secondary"
      :aria-label="t('topbar.search')"
      @click="globalSearchOpen = true"
    >
      <AppIcon name="Search" :size="20" />
    </Button>
    <div class="hidden sm:contents">
      <Button
        text
        rounded
        severity="secondary"
        :aria-label="t('library.title')"
        @click.stop="libraryStore.openPanel()"
      >
        <AppIcon name="LayoutGrid" :size="20" />
      </Button>
    </div>

    <!-- Live operations button — temporary tile that only appears
         while installs/uploads are running, like a Windows 11 taskbar
         progress tile. -->
    <LiveOperationsButton />
    <div class="hidden sm:contents">
      <Button
        text
        rounded
        severity="secondary"
        :aria-label="t('topbar.terminal')"
        @click="openTerminal"
      >
        <AppIcon name="Terminal" :size="20" />
      </Button>
    </div>
    <div class="relative hidden sm:block">
      <Button
        text
        rounded
        severity="secondary"
        :aria-label="t('topbar.settings')"
        @click="openSettings"
      >
        <AppIcon name="Settings" :size="20" />
      </Button>
      <!-- Tiny green dot when a system update is available. Draws the
           user's eye toward Settings → About where the install UI lives. -->
      <span
        v-if="updatesStore.status.updateAvailable && !updatesStore.isActive"
        class="pointer-events-none absolute right-1 top-1 flex h-2.5 w-2.5"
      >
        <span class="absolute inline-flex h-full w-full animate-ping rounded-full bg-emerald-400 opacity-75" />
        <span class="relative inline-flex h-2.5 w-2.5 rounded-full bg-emerald-500" />
      </span>
    </div>

    <!-- Overflow menu: only below `sm`, where it stands in for the icons
         hidden above (see moreMenuItems). -->
    <div class="relative sm:hidden">
      <Button
        text
        rounded
        severity="secondary"
        :aria-label="t('topbar.more')"
        :title="t('topbar.more')"
        aria-haspopup="true"
        @click="openMoreMenu"
      >
        <AppIcon name="MoreHorizontal" :size="20" />
      </Button>
      <span
        v-if="moreMenuHasActivity"
        class="pointer-events-none absolute right-1 top-1 flex h-2.5 w-2.5"
      >
        <span class="relative inline-flex h-2.5 w-2.5 rounded-full bg-blue-500" />
      </span>
    </div>
    <Menu ref="moreMenu" :model="moreMenuItems" :popup="true">
      <template #item="{ item, props }">
        <a
          v-bind="props.action"
          class="flex cursor-pointer items-center gap-2 px-3 py-2 text-sm text-[var(--cn-text)]"
        >
          <AppIcon :name="item.iconName" :size="15" />
          <span>{{ item.label }}</span>
        </a>
      </template>
    </Menu>

    <!-- Notification bell with unread badge. The badge only renders when
         unreadCount > 0 so the icon stays clean when there's nothing new. -->
    <div class="relative">
      <Button
        text
        rounded
        severity="secondary"
        :aria-label="t('topbar.notifications')"
        @click="toggleNotifications"
      >
        <AppIcon name="Bell" :size="20" />
      </Button>
      <Badge
        v-if="notifications.unreadCount > 0"
        :value="notifications.unreadCount > 99 ? '99+' : notifications.unreadCount"
        severity="danger"
        class="pointer-events-none absolute -right-1 -top-1"
      />
    </div>

    <!-- User avatar menu. The default avatar is the CasaNova house+orb
         illustration from casanova.png — used until the user sets their
         own profile picture. PrimeVue Menu's `popup` mode anchors the
         dropdown to whatever element called toggle(). -->
    <button
      class="ml-1 flex shrink-0 items-center gap-2 rounded-full p-1 sm:ml-2 hover:bg-slate-100 dark:hover:bg-slate-800"
      :aria-label="t('topbar.user_menu')"
      @click="openUserMenu"
    >
      <Avatar
        :image="auth.avatarSrc"
        shape="circle"
        class="!bg-transparent"
      />
    </button>
    <Menu ref="userMenu" :model="userMenuItems" :popup="true">
      <template #item="{ item, props }">
        <a
          v-bind="props.action"
          :class="[
            'flex cursor-pointer items-center gap-2 px-3 py-2 text-sm',
            item.kind === 'shutdown' ? 'text-red-500' : item.kind === 'logout' ? 'text-red-500' : 'text-[var(--cn-text)]',
          ]"
        >
          <AppIcon v-if="item.kind === 'account'" name="User" :size="15" />
          <AppIcon v-else-if="item.kind === 'about'" name="Info" :size="15" />
          <AppIcon v-else-if="item.kind === 'restart'" name="RotateCw" :size="15" />
          <AppIcon v-else-if="item.kind === 'shutdown'" name="Power" :size="15" />
          <AppIcon v-else-if="item.kind === 'logout'" name="LogOut" :size="15" />
          <span>{{ item.label }}</span>
        </a>
      </template>
    </Menu>

    <TerminalDialog v-model:visible="terminalVisible" />
    <GlobalSearch v-model:visible="globalSearchOpen" />
    <UserAccountDialog v-model:visible="accountVisible" />
    <ShutdownConfirmDialog v-model:visible="shutdownDialogVisible" :mode="shutdownMode" />
    <AppLibraryPanel
      v-model:visible="libraryStore.panelOpen"
      @custom-install="onCustomInstallFromLibrary"
    />
    <InstallAppDialog
      v-model:visible="customInstallDialogVisible"
      :prefill-yaml="customInstallYaml"
    />
  </header>
</template>
