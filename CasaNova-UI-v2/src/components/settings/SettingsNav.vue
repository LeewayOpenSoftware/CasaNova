<script setup lang="ts">
// SettingsNav — the vertical tab rail for the settings section. Every
// entry is a router-link, so deep linking / browser back / middle-click
// all work naturally without wiring extra handlers.
//
// The list is grouped into two buckets:
//   1. Core — things every user cares about (general, users, storage,
//      containers, network, security)
//   2. Advanced — power-user features (backups, logs, audit, diagnostics)
// Groups are separated by a hairline above a small label, matching how
// most modern settings UIs (macOS Ventura+, GNOME Settings, VS Code)
// present their sections. The hairline is drawn in the template below:
// this comment used to describe one that was never there, leaving the
// two groups distinguished only by a muted label that reads like just
// another row in a list of twenty-two.

import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import { getMigrationReport } from '@/api/migration'
import { getScan as getMaintenanceScan, isModuleAbsent } from '@/api/maintenance'
import { safeCall } from '@/api/safeCall'
import { useAuthStore } from '@/stores/auth'
import { canAccessSettingsPath, effectiveRole } from '@/router/settingsAccess'

interface SettingsNavEntry {
  to: string
  labelKey: string
  icon: string
}

interface SettingsNavSection {
  labelKey: string
  entries: SettingsNavEntry[]
}

const { t } = useI18n()
const auth = useAuthStore()

// Same question the router guard asks, so the rail never offers a page the
// guard would immediately bounce the visitor out of. A non-admin is not being
// denied anything here: those pages load nothing without PermSystemConfig.
const role = computed(() => effectiveRole(auth.accessToken, auth.user?.role))

// Probe the migration report once at mount. Fresh CasaNova installs
// (no /etc/casaos/, no casaos-* units, no legacy compose stacks) get
// the Migration nav entry hidden — putting it back the moment any
// legacy state is detected. Avoids cluttering the sidebar for the
// vast majority of users while keeping the page reachable via direct
// URL for power users debugging an upgrade.
const showMigration = ref(false)

// Maintenance can be absent from the BUILD, not just from this user's reach:
// route/v1.go registers its whole group behind a compile-time constant that is
// currently false, so every path answers 404 and the page itself renders an
// honest "not available in this build" card. That card is the right answer for
// someone who arrives with a bookmark; it is the wrong thing for the rail to
// steer people into. Offering a section whose only content is an apology is
// still offering it.
//
// Probed rather than hard-coded, which matters more than it looks: mirroring a
// backend constant in the frontend would silently become a lie the day the
// constant flips, and nobody would think to come back here. Asking the backend
// means this rail is right in both builds without being edited again. Same
// shape as the migration probe below it, which is why it lives beside it.
//
// Shown until proven absent: hiding on any error would drop a working entry
// during a blip, and only 404/405 actually means "this build does not have it".
const showMaintenance = ref(true)

onMounted(async () => {
  const result = await safeCall(() => getMaintenanceScan())
  if (isModuleAbsent(result.status)) showMaintenance.value = false
})

onMounted(async () => {
  const result = await safeCall(() => getMigrationReport())
  if (result.error || !result.data) return
  const r = result.data
  showMigration.value =
    r.legacy_config_dir ||
    r.configs_migrated.length > 0 ||
    r.legacy_compose_apps.length > 0 ||
    r.legacy_containers > 0 ||
    r.legacy_units.length > 0
})

const sections = computed<SettingsNavSection[]>(() => {
  const advanced: SettingsNavEntry[] = [
    { to: '/app/settings/alerts', labelKey: 'settings.nav.alerts', icon: 'BellRing' },
    { to: '/app/settings/backups', labelKey: 'settings.nav.backups', icon: 'CloudUpload' },
    { to: '/app/settings/scheduled', labelKey: 'settings.nav.scheduled', icon: 'Clock' },
    { to: '/app/settings/logs', labelKey: 'settings.nav.logs', icon: 'ScrollText' },
    { to: '/app/settings/audit', labelKey: 'settings.nav.audit', icon: 'FileText' },
    { to: '/app/settings/troubleshooter', labelKey: 'settings.nav.troubleshooter', icon: 'Wrench' },
    { to: '/app/settings/diagnosis', labelKey: 'settings.nav.diagnosis', icon: 'Activity' },
  ]
  if (showMaintenance.value) {
    // Kept in its original slot rather than appended, so a build that HAS the
    // module shows the same rail order it always did.
    advanced.splice(6, 0, {
      to: '/app/settings/maintenance',
      labelKey: 'settings.nav.maintenance',
      icon: 'Sparkles',
    })
  }
  if (showMigration.value) {
    advanced.push({ to: '/app/settings/migration', labelKey: 'settings.nav.migration', icon: 'ArrowRightLeft' })
  }
  const visible = (entries: SettingsNavEntry[]) =>
    entries.filter((e) => canAccessSettingsPath(e.to, role.value))
  return [
    {
      labelKey: 'settings.nav.core',
      entries: visible([
        { to: '/app/settings/general', labelKey: 'settings.nav.general', icon: 'Settings' },
        // Theme packs used to be its own entry. It is now a section inside
        // Appearance: picking a theme and managing the packs they come from
        // are one errand, and splitting them made you navigate between two
        // pages to finish it.
        { to: '/app/settings/appearance', labelKey: 'settings.nav.appearance_themes', icon: 'Paintbrush' },
        { to: '/app/settings/users', labelKey: 'settings.nav.users', icon: 'Users' },
        { to: '/app/settings/storage', labelKey: 'settings.nav.storage', icon: 'HardDrive' },
        { to: '/app/settings/apps', labelKey: 'settings.nav.apps', icon: 'LayoutGrid' },
        { to: '/app/settings/containers', labelKey: 'settings.nav.containers', icon: 'Container' },
        { to: '/app/settings/app-stores', labelKey: 'settings.nav.app_stores', icon: 'Store' },
        { to: '/app/settings/network', labelKey: 'settings.nav.network', icon: 'Network' },
        { to: '/app/settings/remote-access', labelKey: 'settings.nav.remote_access', icon: 'Globe' },
        { to: '/app/settings/security', labelKey: 'settings.nav.security', icon: 'Shield' },
        { to: '/app/settings/server', labelKey: 'settings.nav.server', icon: 'Server' },
        { to: '/app/settings/host', labelKey: 'settings.nav.host', icon: 'TerminalSquare' },
        { to: '/app/settings/updates', labelKey: 'settings.nav.updates', icon: 'RefreshCcw' },
        // About sat at the very bottom, after the audit log and the
        // diagnostics — which made "what version am I running" a trip
        // through the power-user shelf. It is a basic question, so it
        // closes the basic group instead.
        { to: '/app/settings/about', labelKey: 'settings.nav.about', icon: 'Info' },
      ]),
    },
    { labelKey: 'settings.nav.advanced', entries: visible(advanced) },
    // A section filtered down to nothing would still draw its rule and its
    // header — a group label with no group under it.
  ].filter((section) => section.entries.length > 0)
})
</script>

<template>
  <nav class="cn-widget flex flex-col gap-4 p-2 sm:p-3 md:gap-5">
    <template v-for="(section, i) in sections" :key="section.labelKey">
      <!-- The rule between groups. Only between: a line above the first
           label would fence the rail off from its own container. -->
      <div v-if="i > 0" class="mx-2 border-t border-[var(--cn-border)] sm:mx-3" />

      <div class="flex flex-col gap-1">
        <!-- Section header. Small, uppercase, muted — lives above the
             entries to group them visually. -->
        <div class="px-2 pb-1 text-[10px] font-semibold uppercase tracking-wider text-[var(--cn-text-muted)] sm:px-3">
          {{ t(section.labelKey) }}
        </div>

        <router-link
          v-for="entry in section.entries"
          :key="entry.to"
          :to="entry.to"
          active-class="cn-nav-active"
          class="flex items-center gap-2.5 rounded-lg px-2 py-2 text-sm text-[var(--cn-text-muted)] transition-colors hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)] sm:gap-3 sm:px-3"
        >
          <AppIcon :name="entry.icon" :size="17" class="shrink-0" />
          <span class="truncate">{{ t(entry.labelKey) }}</span>
        </router-link>
      </div>
    </template>
  </nav>
</template>

<style>
/* Active route styling lives in the global layer so it can use the
   CasaNova blue accent from the CSS variables. `.cn-nav-active` is
   applied by vue-router-link when the target matches the current route. */
.cn-nav-active {
  background: rgb(59 130 246 / 0.12);
  color: rgb(96 165 250) !important;
  font-weight: 500;
}
</style>
