<script setup lang="ts">
// AptUpdateCard — OS package (apt) updates for the host distro. Extracted from
// HostSettings so it lives on the dedicated Updates page next to the CasaNova
// self-updater. Check → simulate, upgrade-all / security-only (transient
// service), and the unattended-upgrades toggle.

import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import Button from 'primevue/button'
import Dialog from 'primevue/dialog'
import Tag from 'primevue/tag'
import ToggleSwitch from 'primevue/toggleswitch'
import { useToast } from 'primevue/usetoast'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { safeCall } from '@/api/safeCall'
import { hostApi } from '@/api'
import type { AptStatus, AptUpgradeStatus, AptAutoStatus } from '@/api/host'

const { t } = useI18n()
const toast = useToast()
function ok(s: string, d?: string) { toast.add({ severity: 'success', summary: s, detail: d, life: 3000 }) }
function err(s: string, d?: string) { toast.add({ severity: 'error', summary: s, detail: d, life: 5000 }) }

const apt = ref<AptStatus | null>(null)
const aptChecking = ref(false)
const upgradeConfirm = ref(false)
const upgrade = ref<AptUpgradeStatus | null>(null)
const upgrading = ref(false)
const aptAuto = ref<AptAutoStatus | null>(null)
let aptPoll: ReturnType<typeof setInterval> | null = null

async function loadApt() { const r = await safeCall(() => hostApi.getAptUpdates()); if (!r.error) { apt.value = r.data; aptChecking.value = r.data.checking } }
async function loadAptAuto() { const r = await safeCall(() => hostApi.getAptAuto()); if (!r.error) aptAuto.value = r.data }
async function toggleAutoUpgrade(v: boolean) {
  const r = await safeCall(() => hostApi.setAptAuto(v)); if (r.error) return err(t('settings.host.auto_error'), r.error)
  aptAuto.value = r.data; ok(v ? t('settings.host.auto_on') : t('settings.host.auto_off'))
}
async function checkApt() {
  aptChecking.value = true
  const r = await safeCall(() => hostApi.runAptCheck())
  if (r.error) { aptChecking.value = false; return err(t('settings.host.apt_check_error'), r.error) }
  const poll = setInterval(async () => { await loadApt(); if (!aptChecking.value) clearInterval(poll) }, 3000)
}
const dockerInUpgrade = computed(() => !!apt.value && apt.value.packages.some((p) => /docker|containerd|linux-image|linux-headers/.test(p.name)))
async function startUpgrade() {
  upgradeConfirm.value = false; upgrading.value = true
  const r = await safeCall(() => hostApi.runAptUpgrade())
  if (r.error) { upgrading.value = false; return err(t('settings.host.apt_upgrade_error'), r.error) }
  toast.add({ severity: 'info', summary: t('settings.host.apt_upgrade_started'), life: 3000 })
  aptPoll = setInterval(pollUpgrade, 3000); pollUpgrade()
}
async function pollUpgrade() {
  const r = await safeCall(() => hostApi.getAptUpgradeStatus()); if (r.error) return
  upgrade.value = r.data
  if (r.data.state === 'success' || r.data.state === 'failed') {
    if (aptPoll) { clearInterval(aptPoll); aptPoll = null }
    if (upgrading.value) {
      upgrading.value = false
      if (r.data.state === 'success') { ok(t('settings.host.apt_upgrade_done')); loadApt() }
      else err(t('settings.host.apt_upgrade_failed'), `exit ${r.data.exitCode}`)
    }
  }
}
async function runSecurityNow() {
  upgrading.value = true
  const r = await safeCall(() => hostApi.runSecurityNow())
  if (r.error) { upgrading.value = false; return err(t('settings.host.apt_upgrade_error'), r.error) }
  toast.add({ severity: 'info', summary: t('settings.host.security_started'), life: 3000 })
  aptPoll = setInterval(pollUpgrade, 3000); pollUpgrade()
}

onMounted(() => {
  loadApt(); loadAptAuto()
  pollUpgrade().then(() => { if (upgrade.value?.state === 'running') { upgrading.value = true; aptPoll = setInterval(pollUpgrade, 3000) } })
})
onUnmounted(() => { if (aptPoll) clearInterval(aptPoll) })
// A repository URL is long and the interesting part is the tail (the PPA owner
// and name), so show that and keep the full list in the tooltip.
function shortRepo(url: string): string {
  try {
    const u = new URL(url)
    const tail = u.pathname.split('/').filter(Boolean).slice(0, 2).join('/')
    return tail ? `${u.host}/${tail}` : u.host
  } catch {
    return url
  }
}

</script>

<template>
  <SettingsCard :title="t('settings.host.apt_title')" :description="t('settings.host.apt_desc')">
    <template #actions>
      <Button size="small" severity="secondary" :loading="aptChecking" :disabled="!apt || !apt.supported" @click="checkApt">
        <AppIcon name="RefreshCw" :size="15" class="mr-1.5" />{{ t('settings.host.apt_check') }}
      </Button>
    </template>
    <div v-if="apt && !apt.supported" class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.apt_unsupported') }}</div>
    <div v-else-if="apt" class="flex flex-col gap-3">
      <div v-if="apt.rebootRequired" class="flex items-center gap-2 rounded-lg bg-amber-500/10 px-3 py-2 text-xs text-amber-500">
        <AppIcon name="RotateCcw" :size="15" />{{ t('settings.host.apt_reboot') }}
      </div>
      <div class="flex items-center gap-2 text-sm">
        <span class="font-medium text-[var(--cn-text)]">{{ t('settings.host.apt_count', { n: apt.total }, apt.total) }}</span>
        <Tag v-if="apt.security > 0" severity="danger" :value="t('settings.host.apt_security', { n: apt.security })" />
        <span v-if="apt.stale" class="text-xs text-amber-500">· {{ t('settings.host.apt_stale') }}</span>
        <!-- A partial refresh is not an alarm, it is a fact the operator has to
             act on: one dead repository while the rest are current. Naming it is
             the whole point — "lists could not be refreshed" sent them looking
             for a network problem that did not exist. -->
        <span
          v-else-if="apt.failedRepos?.length"
          class="truncate text-xs text-[var(--cn-text-muted)]"
          :title="apt.failedRepos.join('\n')"
        >· {{ t('settings.host.apt_repo_failed', { n: apt.failedRepos.length, repo: shortRepo(apt.failedRepos[0]) }, apt.failedRepos.length) }}</span>
        <span v-if="apt.checkedAt" class="ml-auto text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.apt_checked', { when: new Date(apt.checkedAt).toLocaleString() }) }}</span>
      </div>
      <div v-if="apt.packages.length" class="max-h-56 overflow-y-auto rounded-lg border border-[var(--cn-border)]">
        <div v-for="p in apt.packages" :key="p.name" class="flex items-center gap-2 border-b border-[var(--cn-border)] px-3 py-1.5 text-xs last:border-b-0">
          <AppIcon :name="p.security ? 'ShieldAlert' : 'Package'" :size="14" :class="p.security ? 'text-red-500' : 'text-[var(--cn-text-muted)]'" />
          <span class="font-mono text-[var(--cn-text)]">{{ p.name }}</span>
          <span class="ml-auto font-mono text-[var(--cn-text-muted)]">{{ p.currentVersion || '—' }} → {{ p.newVersion }}</span>
        </div>
      </div>
      <div class="flex flex-wrap items-center gap-3">
        <Button v-if="apt.total > 0" size="small" severity="warn" :loading="upgrading" @click="upgradeConfirm = true">
          <AppIcon name="Download" :size="15" class="mr-1.5" />{{ t('settings.host.apt_upgrade_all') }}
        </Button>
        <Button v-if="apt.security > 0" size="small" severity="secondary" :loading="upgrading" @click="runSecurityNow">
          <AppIcon name="ShieldCheck" :size="15" class="mr-1.5" />{{ t('settings.host.security_now') }}
        </Button>
        <label v-if="aptAuto?.supported" class="ml-auto flex items-center gap-2 text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.host.auto_label') }}
          <ToggleSwitch :model-value="aptAuto.enabled" @update:model-value="toggleAutoUpgrade" />
        </label>
      </div>
      <!-- Indeterminate progress while the transient apt service runs (dpkg
           gives no clean %); the log tail below carries the detail. -->
      <div v-if="upgrading" class="flex flex-col gap-1">
        <span class="text-[11px] text-blue-500">{{ t('settings.host.apt_upgrade_running') }}</span>
        <div class="h-1 w-full overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
          <div class="cn-indeterminate h-full w-1/3 rounded-full bg-blue-500" />
        </div>
      </div>
      <pre v-if="upgrade && upgrade.tail.length && (upgrading || upgrade.state !== 'idle')" class="max-h-40 overflow-y-auto rounded-lg bg-black/60 p-3 font-mono text-[11px] leading-relaxed text-green-300">{{ upgrade.tail.join('\n') }}</pre>
    </div>
  </SettingsCard>

  <Dialog v-model:visible="upgradeConfirm" modal :header="t('settings.host.apt_confirm_title')" :style="{ width: '30rem' }">
    <div class="flex flex-col gap-3 text-sm">
      <p class="text-[var(--cn-text-muted)]">{{ t('settings.host.apt_confirm_body', { n: apt?.total ?? 0 }) }}</p>
      <div v-if="dockerInUpgrade" class="flex items-start gap-2 rounded-lg bg-red-500/10 px-3 py-2 text-xs text-red-500">
        <AppIcon name="TriangleAlert" :size="16" class="mt-0.5 shrink-0" /><span>{{ t('settings.host.apt_confirm_docker') }}</span>
      </div>
    </div>
    <template #footer>
      <Button severity="secondary" text @click="upgradeConfirm = false">{{ t('common.cancel') }}</Button>
      <Button severity="warn" @click="startUpgrade">{{ t('settings.host.apt_confirm_go') }}</Button>
    </template>
  </Dialog>
</template>
