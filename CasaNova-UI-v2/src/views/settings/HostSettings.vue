<script setup lang="ts">
// Host settings — administer the Linux underneath CasaNova. Five cards:
// CPU & memory, storage, services, monitoring & power, advanced sysctls.
// Related controls share a card and pair up two-per-row on wide screens;
// one control per full-width card meant scrolling past a stack of near-
// empty headers to reach anything. Reads are safe; every write is
// validated server-side against what the kernel/system exposes.
// Dangerous actions (apt upgrade, service disable) confirm first.

import { skipWhenHidden } from '@/composables/skipWhenHidden'
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import StatusChip from '@/components/ui/StatusChip.vue'
import Select from 'primevue/select'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import Dialog from 'primevue/dialog'
import ToggleSwitch from 'primevue/toggleswitch'
import { useToast } from 'primevue/usetoast'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { safeCall } from '@/api/safeCall'
import { hostApi } from '@/api'
import type {
  CPUGovernorInfo, SwappinessInfo, HostMemoryInfo,
  DiskScheduler, SysctlEntry, ZramInfo, SensorReport,
  HostServiceStatus, UPSInfo, ProcInfo,
  PowerStatus, PowerConfig,
} from '@/api/host'

const { t } = useI18n()
const toast = useToast()

function fmtBytes(b: number): string {
  if (b < 0) return '—'
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0, n = b
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++ }
  return `${n.toFixed(n < 10 && i > 0 ? 1 : 0)} ${u[i]}`
}
function ok(summary: string, detail?: string) { toast.add({ severity: 'success', summary, detail, life: 3000 }) }
function err(summary: string, detail?: string) { toast.add({ severity: 'error', summary, detail, life: 5000 }) }

// ── CPU governor + EPP ───────────────────────────────────────────────
const gov = ref<CPUGovernorInfo | null>(null)
const govSaving = ref(false)
async function loadGov() { const r = await safeCall(() => hostApi.getCPUGovernor()); if (!r.error) gov.value = r.data }
async function saveGov(value: string) {
  if (!gov.value || value === gov.value.current) return
  govSaving.value = true
  const r = await safeCall(() => hostApi.setCPUGovernor(value)); govSaving.value = false
  if (r.error) return err(t('settings.host.gov_error'), r.error)
  gov.value = r.data; ok(t('settings.host.gov_saved'), value)
}
async function saveEPP(value: string) {
  if (!gov.value || value === gov.value.epp) return
  const r = await safeCall(() => hostApi.setEPP(value))
  if (r.error) return err(t('settings.host.epp_error'), r.error)
  gov.value = r.data; ok(t('settings.host.epp_saved'), value)
}

// ── Swappiness ───────────────────────────────────────────────────────
const swap = ref<SwappinessInfo | null>(null)
const swapVal = ref(60)
const swapSaving = ref(false)
async function loadSwap() { const r = await safeCall(() => hostApi.getSwappiness()); if (!r.error) { swap.value = r.data; swapVal.value = r.data.value } }
async function saveSwap(value: number) {
  swapSaving.value = true
  const r = await safeCall(() => hostApi.setSwappiness(value)); swapSaving.value = false
  if (r.error) return err(t('settings.host.swap_error'), r.error)
  swap.value = r.data; swapVal.value = r.data.value; ok(t('settings.host.swap_saved'), String(value))
}

// ── Docker memory cap ────────────────────────────────────────────────
const mem = ref<HostMemoryInfo | null>(null)
const memInput = ref('')
const memSaving = ref(false)
const memLimited = computed(() => !!mem.value && mem.value.memoryMax !== 'infinity')
const floorLabel = computed(() => mem.value ? `${Math.round(mem.value.floorMiB / 1024 * 10) / 10} GiB` : '')
async function loadMem() {
  const r = await safeCall(() => hostApi.getHostMemory())
  if (!r.error) { mem.value = r.data; memInput.value = r.data.memoryMax === 'infinity' ? '' : r.data.memoryMax }
}
async function applyMem(value: string) {
  memSaving.value = true
  const r = await safeCall(() => hostApi.setHostMemory(value)); memSaving.value = false
  if (r.error) return err(t('settings.host.mem_error'), r.error)
  mem.value = r.data; memInput.value = r.data.memoryMax === 'infinity' ? '' : r.data.memoryMax; ok(t('settings.host.mem_saved'))
}

// ── zram ─────────────────────────────────────────────────────────────
const zram = ref<ZramInfo | null>(null)
const zramSize = ref(0)
const zramSaving = ref(false)
async function loadZram() { const r = await safeCall(() => hostApi.getZram()); if (!r.error) { zram.value = r.data; if (!zramSize.value) zramSize.value = r.data.diskSizeMiB || Math.round((r.data.memTotalMiB || 4096) / 4) } }
async function toggleZram(enable: boolean) {
  if (!zram.value) return
  zramSaving.value = true
  const r = await safeCall(() => hostApi.setZram(enable, zram.value!.algorithm || 'zstd', zramSize.value)); zramSaving.value = false
  if (r.error) return err(t('settings.host.zram_error'), r.error)
  zram.value = r.data; ok(enable ? t('settings.host.zram_on') : t('settings.host.zram_off'))
}

// ── I/O scheduler + spindown ─────────────────────────────────────────
const disks = ref<DiskScheduler[]>([])
async function loadDisks() { const r = await safeCall(() => hostApi.getIOSchedulers()); if (!r.error) disks.value = r.data }
async function setDiskSched(disk: string, scheduler: string) {
  const r = await safeCall(() => hostApi.setIOScheduler(disk, scheduler)); if (r.error) return err(t('settings.host.io_error'), r.error)
  disks.value = r.data; ok(t('settings.host.io_saved'), `${disk}: ${scheduler}`)
}
const spindownOptions = ['off', '10m', '20m', '30m', '1h']
async function setDiskSpindown(disk: string, timeout: string) {
  const r = await safeCall(() => hostApi.setSpindown(disk, timeout)); if (r.error) return err(t('settings.host.spindown_error'), r.error)
  disks.value = r.data; ok(t('settings.host.spindown_saved'), `${disk}: ${timeout}`)
}

// ── Sysctl (advanced) ────────────────────────────────────────────────
const sysctls = ref<SysctlEntry[]>([])
const sysctlEdits = ref<Record<string, string>>({})
const showSysctl = ref(false)
async function loadSysctls() {
  const r = await safeCall(() => hostApi.getSysctls())
  if (!r.error) { sysctls.value = r.data; r.data.forEach((s) => { sysctlEdits.value[s.key] = s.value }) }
}
async function saveSysctl(key: string) {
  const r = await safeCall(() => hostApi.setSysctl(key, sysctlEdits.value[key])); if (r.error) return err(t('settings.host.sysctl_error'), r.error)
  sysctls.value = r.data; r.data.forEach((s) => { sysctlEdits.value[s.key] = s.value }); ok(t('settings.host.sysctl_saved'), key)
}

// APT (OS package) updates moved to the dedicated Updates page
// (components/settings/AptUpdateCard.vue) alongside the CasaNova self-updater.

// ── Services ─────────────────────────────────────────────────────────
const services = ref<HostServiceStatus[]>([])
const svcConfirm = ref<{ svc: HostServiceStatus; action: 'restart' | 'disable' } | null>(null)
async function loadServices() { const r = await safeCall(() => hostApi.getServices()); if (!r.error) services.value = r.data }
async function doServiceAction(key: string, action: 'restart' | 'enable' | 'disable') {
  svcConfirm.value = null
  const r = await safeCall(() => hostApi.serviceAction(key, action)); if (r.error) return err(t('settings.host.svc_error'), r.error)
  services.value = r.data; ok(t('settings.host.svc_done'), `${key}: ${action}`)
}

// ── Monitoring: sensors, UPS, processes (polled) ─────────────────────
const sensors = ref<SensorReport | null>(null)
const ups = ref<UPSInfo | null>(null)
const procs = ref<ProcInfo[]>([])
const upsHost = ref('')
const upsName = ref('')
const upsSaving = ref(false)
const showUpsConfig = ref(false)
// UPS-aware power orchestrator (Forza daemon). Opt-in.
const power = ref<PowerStatus | null>(null)
const powerCfg = ref<PowerConfig | null>(null)
const powerSaving = ref(false)
const showPowerConfig = ref(false)
let monPoll: ReturnType<typeof setInterval> | null = null
// The 5s monitoring tick fans out to four expensive host calls (sensors, UPS,
// processes, power). Without this guard a slow tick let the next one start on
// top, and under load they stacked until the box was answering nothing else.
let monInFlight = false
async function loadMon() {
  if (monInFlight) return
  if (typeof document !== 'undefined' && document.hidden) return
  monInFlight = true
  try {
    const [s, u, p, pw] = await Promise.all([
      safeCall(() => hostApi.getSensors()),
      safeCall(() => hostApi.getUPS()),
      safeCall(() => hostApi.getProcesses(15)),
      safeCall(() => hostApi.getPower()),
    ])
    if (!s.error) sensors.value = s.data
    if (!u.error) ups.value = u.data
    if (!p.error) procs.value = p.data
    if (!pw.error) power.value = pw.data
  } finally {
    monInFlight = false
  }
}
async function loadPowerConfig() {
  const r = await safeCall(() => hostApi.getPowerConfig())
  if (!r.error) powerCfg.value = r.data
}
async function savePowerConfig() {
  if (!powerCfg.value) return
  powerSaving.value = true
  const r = await safeCall(() => hostApi.setPowerConfig(powerCfg.value!))
  powerSaving.value = false
  if (r.error) return err(t('settings.host.power_save_error'), r.error)
  powerCfg.value = r.data
  ok(t('settings.host.power_saved'))
}
// advice → StatusChip tone
function adviceTone(advice: string): 'success' | 'warning' | 'danger' {
  if (advice === 'shutdown_now') return 'danger'
  if (advice === 'prepare_shutdown') return 'warning'
  return 'success'
}
// Comma-separated text ⇄ string[] bridges for the list fields.
function listToText(list?: string[]): string { return (list ?? []).join(', ') }
function textToList(text: string): string[] {
  return text.split(',').map(s => s.trim()).filter(s => s.length > 0)
}
const essentialAppsText = computed({
  get: () => listToText(powerCfg.value?.essentialApps),
  set: (v: string) => { if (powerCfg.value) powerCfg.value.essentialApps = textToList(v) },
})
const spindownDisksText = computed({
  get: () => listToText(powerCfg.value?.spindownDisks),
  set: (v: string) => { if (powerCfg.value) powerCfg.value.spindownDisks = textToList(v) },
})
async function loadUpsConfig() {
  const r = await safeCall(() => hostApi.getUPSConfig())
  if (!r.error) { upsHost.value = r.data.host; upsName.value = r.data.upsName }
}
async function saveUpsConfig() {
  upsSaving.value = true
  const r = await safeCall(() => hostApi.setUPSConfig(upsHost.value.trim(), upsName.value.trim()))
  upsSaving.value = false
  if (r.error) return err(t('settings.host.ups_save_error'), r.error)
  ups.value = r.data; showUpsConfig.value = false
  ok(r.data.detected ? t('settings.host.ups_connected') : t('settings.host.ups_saved'), r.data.error || undefined)
}

onMounted(() => {
  loadGov(); loadSwap(); loadMem(); loadZram(); loadDisks(); loadSysctls()
  loadServices(); loadMon(); loadUpsConfig(); loadPowerConfig()
  monPoll = setInterval(skipWhenHidden(loadMon), 5000)
})
onUnmounted(() => { if (monPoll) clearInterval(monPoll) })
</script>

<template>
  <SettingsPage icon="Server" :title="t('settings.host.title')" :description="t('settings.host.description')">
    <!-- ═══ CPU + memory ═══
         Governor, swappiness, the Docker cap and zram used to be four
         full-width cards with one control each, so reaching anything below
         them meant scrolling past four near-empty headers. They share one
         card now and pair up two-per-row as soon as there is width for it. -->
    <SettingsCard :title="t('settings.host.group_cpu_memory')">
      <div class="grid gap-x-8 gap-y-6 lg:grid-cols-2">
        <section class="flex flex-col gap-3">
          <div>
            <h3 class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.host.gov_title') }}</h3>
            <p class="mt-0.5 text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.gov_desc') }}</p>
          </div>
          <div v-if="gov && gov.supported" class="flex flex-col gap-3">
            <div class="flex flex-wrap items-center gap-3">
              <AppIcon name="Cpu" :size="18" class="text-[var(--cn-text-muted)]" />
              <Select :model-value="gov.current" :options="gov.available" :disabled="govSaving" :placeholder="gov.current" class="w-56" @update:model-value="saveGov" />
              <span v-if="gov.current === 'mixed'" class="text-xs text-amber-500">{{ t('settings.host.gov_mixed') }}</span>
              <!-- EPP (P3) — only when the CPU exposes it -->
              <template v-if="gov.eppAvailable && gov.eppAvailable.length">
                <span class="text-xs text-[var(--cn-text-muted)]">EPP</span>
                <Select :model-value="gov.epp" :options="gov.eppAvailable" class="w-52" @update:model-value="saveEPP" />
              </template>
            </div>
            <p class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.gov_driver', { driver: gov.driver || '—', cores: gov.perCore.length }) }}</p>
          </div>
          <p v-else class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.gov_unsupported') }}</p>
        </section>

        <section class="flex flex-col gap-3">
          <div>
            <h3 class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.host.swap_title') }}</h3>
            <p class="mt-0.5 text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.swap_desc') }}</p>
          </div>
          <div v-if="swap" class="flex flex-col gap-3">
            <div class="flex items-center gap-4">
              <input v-model.number="swapVal" type="range" min="0" max="100" step="1" :disabled="swapSaving" class="h-2 flex-1 cursor-pointer accent-blue-500" @change="saveSwap(swapVal)" />
              <span class="w-10 text-right font-mono text-sm text-[var(--cn-text)]">{{ swapVal }}</span>
            </div>
            <div class="flex items-center gap-2">
              <Button size="small" severity="secondary" :disabled="swapSaving" @click="saveSwap(10)">{{ t('settings.host.swap_nas') }}</Button>
              <Button size="small" severity="secondary" :disabled="swapSaving" @click="saveSwap(60)">{{ t('settings.host.swap_default') }}</Button>
              <span class="ml-auto text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.swap_total', { total: fmtBytes(swap.swapTotalKiB * 1024) }) }}</span>
            </div>
          </div>
        </section>

        <section class="flex flex-col gap-3">
          <div>
            <h3 class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.host.mem_title') }}</h3>
            <p class="mt-0.5 text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.mem_desc') }}</p>
          </div>
          <div v-if="mem" class="flex flex-col gap-3">
            <div class="flex flex-wrap items-center gap-2">
              <AppIcon name="MemoryStick" :size="18" class="text-[var(--cn-text-muted)]" />
              <InputText v-model="memInput" placeholder="4G" class="w-28 font-mono text-sm" :disabled="memSaving" />
              <Button size="small" :disabled="memSaving || !memInput" :loading="memSaving" @click="applyMem(memInput)">{{ t('common.save') }}</Button>
              <Button v-if="memLimited" size="small" severity="secondary" :disabled="memSaving" @click="applyMem('infinity')">{{ t('settings.host.mem_clear') }}</Button>
            </div>
            <p class="text-xs text-[var(--cn-text-muted)]">
              {{ t('settings.host.mem_state', { limit: memLimited ? mem.memoryMax : t('settings.host.mem_unlimited'), used: fmtBytes(mem.dockerUsedBytes), total: fmtBytes(mem.memTotalKiB * 1024) }) }} · {{ t('settings.host.mem_floor', { floor: floorLabel }) }}
            </p>
          </div>
        </section>

        <section class="flex flex-col gap-3">
          <div>
            <h3 class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.host.zram_title') }}</h3>
            <p class="mt-0.5 text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.zram_desc') }}</p>
          </div>
          <div v-if="zram" class="flex flex-col gap-3">
            <div class="flex items-center justify-between">
              <div class="flex items-center gap-3">
                <AppIcon name="Zap" :size="18" class="text-[var(--cn-text-muted)]" />
                <span class="text-sm text-[var(--cn-text)]">{{ zram.enabled ? t('settings.host.zram_active', { algo: zram.algorithm, size: zram.diskSizeMiB }) : t('settings.host.zram_inactive') }}</span>
              </div>
              <ToggleSwitch :model-value="zram.enabled" :disabled="zramSaving" @update:model-value="toggleZram" />
            </div>
            <div v-if="!zram.enabled" class="flex items-center gap-3">
              <span class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.zram_size') }}</span>
              <input v-model.number="zramSize" type="range" min="256" :max="zram.memTotalMiB || 8192" step="256" class="h-2 flex-1 cursor-pointer accent-blue-500" />
              <span class="w-20 text-right font-mono text-xs text-[var(--cn-text)]">{{ (zramSize/1024).toFixed(1) }} GiB</span>
            </div>
            <p v-else class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.zram_stats', { orig: zram.origDataMiB, compr: zram.comprDataMiB }) }}</p>
          </div>
        </section>
      </div>
    </SettingsCard>

    <!-- ═══ Storage: I/O scheduler + spindown ═══ -->
    <SettingsCard :title="t('settings.host.io_title')" :description="t('settings.host.io_desc')">
      <div v-if="disks.length" class="flex flex-col gap-3">
        <div v-for="d in disks" :key="d.name" class="flex flex-wrap items-center gap-2 rounded-lg border border-[var(--cn-border)] p-3">
          <AppIcon :name="d.rotational ? 'HardDrive' : 'Zap'" :size="16" class="text-[var(--cn-text-muted)]" />
          <div class="min-w-0">
            <div class="text-sm font-medium text-[var(--cn-text)]">{{ d.name }} <span class="font-normal text-[var(--cn-text-muted)]">{{ d.model }}</span></div>
            <div class="text-xs text-[var(--cn-text-muted)]">{{ fmtBytes(d.sizeBytes) }} · {{ d.rotational ? t('settings.host.io_hdd') : t('settings.host.io_ssd') }}</div>
          </div>
          <div class="ml-auto flex items-center gap-2">
            <Select :model-value="d.current" :options="d.available" class="w-40" @update:model-value="(v: string) => setDiskSched(d.name, v)" />
            <Select v-if="d.rotational" :model-value="d.spindown || 'off'" :options="spindownOptions" class="w-24" @update:model-value="(v: string) => setDiskSpindown(d.name, v)" />
          </div>
        </div>
      </div>
      <p v-else class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.io_none') }}</p>
    </SettingsCard>

    <!-- ═══ Services ═══ -->
    <SettingsCard :title="t('settings.host.svc_title')" :description="t('settings.host.svc_desc')">
      <div class="flex flex-col gap-2">
        <div v-for="s in services.filter(x => x.installed)" :key="s.key" class="flex items-center gap-3 rounded-lg border border-[var(--cn-border)] px-3 py-2">
          <StatusChip dot :tone="s.active ? 'success' : 'neutral'" />
          <div class="min-w-0">
            <div class="text-sm text-[var(--cn-text)]">{{ s.label }}</div>
            <div class="text-xs text-[var(--cn-text-muted)]">{{ s.active ? t('settings.host.svc_running') : t('settings.host.svc_stopped') }}{{ s.enabled ? ' · ' + t('settings.host.svc_boot') : '' }}</div>
          </div>
          <div class="ml-auto flex items-center gap-1.5">
            <Button size="small" severity="secondary" text @click="svcConfirm = { svc: s, action: 'restart' }">{{ t('settings.host.svc_restart') }}</Button>
            <Button v-if="s.canToggle && s.enabled" size="small" severity="danger" text @click="svcConfirm = { svc: s, action: 'disable' }">{{ t('settings.host.svc_disable') }}</Button>
            <Button v-else-if="s.canToggle && !s.enabled" size="small" severity="secondary" text @click="doServiceAction(s.key, 'enable')">{{ t('settings.host.svc_enable') }}</Button>
          </div>
        </div>
      </div>
    </SettingsCard>

    <!-- ═══ Monitoring + power: sensors, UPS, orchestrator, processes ═══
         All four are read-mostly views of the same 5s poll, so they live in
         one card. Sensors and UPS sit side by side; the orchestrator and the
         process table span the full width because their content needs it. -->
    <SettingsCard :title="t('settings.host.group_monitoring')">
      <div class="grid gap-x-8 gap-y-6 lg:grid-cols-2">
        <section v-if="sensors && sensors.supported" class="flex flex-col gap-3">
          <div>
            <h3 class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.host.sensors_title') }}</h3>
            <p class="mt-0.5 text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.sensors_desc') }}</p>
          </div>
          <div class="grid grid-cols-2 gap-3">
            <template v-for="chip in sensors.chips" :key="chip.name">
              <div v-for="tp in chip.temps" :key="chip.name + tp.label" class="rounded-lg border border-[var(--cn-border)] p-2.5">
                <div class="truncate text-xs text-[var(--cn-text-muted)]">{{ chip.name }} · {{ tp.label }}</div>
                <div class="text-lg font-semibold" :class="tp.critC && tp.celsius >= tp.critC - 10 ? 'text-red-500' : 'text-[var(--cn-text)]'">{{ tp.celsius.toFixed(1) }}°C</div>
              </div>
            </template>
            <template v-for="chip in sensors.chips" :key="chip.name + 'f'">
              <div v-for="fn in chip.fans" :key="chip.name + fn.label" class="rounded-lg border border-[var(--cn-border)] p-2.5">
                <div class="truncate text-xs text-[var(--cn-text-muted)]">{{ fn.label }}</div>
                <div class="text-lg font-semibold text-[var(--cn-text)]">{{ fn.rpm }} <span class="text-xs font-normal">RPM</span></div>
              </div>
            </template>
          </div>
        </section>

        <section class="flex flex-col gap-3">
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0">
              <h3 class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.host.ups_title') }}</h3>
              <p class="mt-0.5 text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.ups_desc') }}</p>
            </div>
            <Button size="small" severity="secondary" text class="shrink-0" @click="showUpsConfig = !showUpsConfig">
              <AppIcon name="Settings2" :size="15" class="mr-1" />{{ t('settings.host.ups_configure') }}
            </Button>
          </div>

          <!-- config: point at a remote NUT server (e.g. 192.0.2.10) -->
          <div v-if="showUpsConfig" class="flex flex-col gap-2 rounded-lg border border-[var(--cn-border)] p-3">
            <p class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.ups_remote_desc') }}</p>
            <div class="flex flex-wrap items-center gap-2">
              <InputText v-model="upsHost" :placeholder="t('settings.host.ups_host_ph')" class="w-48 font-mono text-sm" />
              <InputText v-model="upsName" :placeholder="t('settings.host.ups_name_ph')" class="w-32 font-mono text-sm" />
              <Button size="small" :loading="upsSaving" @click="saveUpsConfig">{{ t('common.save') }}</Button>
              <Button v-if="upsHost" size="small" severity="secondary" text @click="upsHost=''; upsName=''; saveUpsConfig()">{{ t('settings.host.ups_local') }}</Button>
            </div>
          </div>

          <div v-if="ups && ups.detected">
            <div class="mb-2 text-xs text-[var(--cn-text-muted)]">
              {{ ups.model || ups.name }}<span v-if="ups.remote"> · {{ t('settings.host.ups_via', { host: ups.host }) }}</span>
            </div>
            <div class="grid grid-cols-2 gap-3">
              <div class="rounded-lg border border-[var(--cn-border)] p-2.5"><div class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.ups_status') }}</div><div class="text-sm font-semibold text-[var(--cn-text)]">{{ ups.status || '—' }}</div></div>
              <div class="rounded-lg border border-[var(--cn-border)] p-2.5"><div class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.ups_charge') }}</div><div class="text-sm font-semibold text-[var(--cn-text)]">{{ ups.chargePct }}%</div></div>
              <div class="rounded-lg border border-[var(--cn-border)] p-2.5"><div class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.ups_load') }}</div><div class="text-sm font-semibold text-[var(--cn-text)]">{{ ups.loadPct }}%</div></div>
              <div class="rounded-lg border border-[var(--cn-border)] p-2.5"><div class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.ups_runtime') }}</div><div class="text-sm font-semibold text-[var(--cn-text)]">{{ Math.round(ups.runtimeSec / 60) }} min</div></div>
            </div>
          </div>
          <p v-else class="text-xs text-[var(--cn-text-muted)]">
            {{ ups?.error || t('settings.host.ups_none') }}
          </p>
        </section>

        <!-- Full width: the orchestrator carries a config form and four
             gauges, and neither survives half a row. -->
        <section class="flex flex-col gap-3 lg:col-span-2">
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0">
              <h3 class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.host.power_title') }}</h3>
              <p class="mt-0.5 text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_desc') }}</p>
            </div>
            <Button size="small" severity="secondary" text class="shrink-0" @click="showPowerConfig = !showPowerConfig">
              <AppIcon name="Settings2" :size="15" class="mr-1" />{{ t('settings.host.power_configure') }}
            </Button>
          </div>

          <div v-if="showPowerConfig && powerCfg" class="flex flex-col gap-3 rounded-lg border border-[var(--cn-border)] p-3">
            <div class="flex items-center justify-between gap-3">
              <div><div class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.host.power_enable') }}</div><div class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_enable_desc') }}</div></div>
              <ToggleSwitch v-model="powerCfg.enabled" />
            </div>
            <div class="flex items-center justify-between gap-3">
              <div><div class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.host.power_autoshutdown') }}</div><div class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_autoshutdown_desc') }}</div></div>
              <ToggleSwitch v-model="powerCfg.autoShutdown" :disabled="!powerCfg.enabled" />
            </div>
            <div class="flex flex-wrap items-center gap-2">
              <label class="w-28 text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_forza_url') }}</label>
              <InputText v-model="powerCfg.forzaUrl" placeholder="http://192.0.2.10:9090" class="w-56 font-mono text-sm" />
            </div>
            <div class="flex flex-wrap items-center gap-2">
              <label class="w-28 text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_forza_token') }}</label>
              <InputText v-model="powerCfg.forzaToken" type="password" :placeholder="t('settings.host.power_forza_token_ph')" class="w-56 font-mono text-sm" />
            </div>
            <p class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_forza_token_desc') }}</p>
            <div class="flex flex-wrap items-center gap-2">
              <label class="w-28 text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_essential_apps') }}</label>
              <InputText v-model="essentialAppsText" :placeholder="t('settings.host.power_essential_apps_ph')" class="w-full max-w-md font-mono text-sm sm:w-96" />
            </div>
            <p class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_essential_apps_desc') }}</p>
            <div class="flex items-center justify-between gap-3">
              <div><div class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.host.power_ladder') }}</div><div class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_ladder_desc') }}</div></div>
              <ToggleSwitch v-model="powerCfg.savingsLadder" :disabled="!powerCfg.enabled" />
            </div>
            <div class="flex flex-wrap items-center gap-2">
              <label class="w-28 text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_spindown_disks') }}</label>
              <InputText v-model="spindownDisksText" :disabled="!powerCfg.savingsLadder" placeholder="sda, sdb" class="w-56 font-mono text-sm" />
            </div>
            <p class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_spindown_disks_desc') }}</p>
            <div class="flex items-center justify-between gap-3">
              <div><div class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.host.power_pause_apps') }}</div><div class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_pause_apps_desc') }}</div></div>
              <ToggleSwitch v-model="powerCfg.pauseNonEssential" :disabled="!powerCfg.savingsLadder" />
            </div>
            <div>
              <Button size="small" :loading="powerSaving" @click="savePowerConfig">{{ t('common.save') }}</Button>
            </div>
          </div>

          <div v-if="power && power.power && power.power.reachable">
            <div class="mb-2 flex flex-wrap items-center gap-2">
              <StatusChip :tone="adviceTone(power.power.advice)">{{ t('settings.host.power_advice_' + power.power.advice) }}</StatusChip>
              <span v-if="power.power.on_battery" class="text-xs font-medium text-[var(--cn-warning)]">{{ t('settings.host.power_on_battery') }}</span>
              <template v-if="power.ladder?.active">
                <StatusChip v-for="lever in power.ladder.levers" :key="lever" tone="warning" size="sm" icon="Leaf">{{ t('settings.host.power_lever_' + lever) }}</StatusChip>
              </template>
              <span class="text-xs text-[var(--cn-text-muted)]">· {{ power.enabled ? t('settings.host.power_active') : t('settings.host.power_observing') }}</span>
            </div>
            <div class="grid grid-cols-2 gap-3 sm:grid-cols-4">
              <div class="rounded-lg border border-[var(--cn-border)] p-2.5"><div class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_soc') }}</div><div class="text-sm font-semibold text-[var(--cn-text)]">{{ Math.round(power.power.soc_pct) }}%</div></div>
              <div class="rounded-lg border border-[var(--cn-border)] p-2.5"><div class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_runtime') }}</div><div class="text-sm font-semibold text-[var(--cn-text)]">{{ Math.round(power.power.est_runtime_min) }} min</div></div>
              <div class="rounded-lg border border-[var(--cn-border)] p-2.5"><div class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_load') }}</div><div class="text-sm font-semibold text-[var(--cn-text)]">{{ Math.round(power.power.load_pct) }}%</div></div>
              <div class="rounded-lg border border-[var(--cn-border)] p-2.5"><div class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.power_confidence') }}</div><div class="text-sm font-semibold text-[var(--cn-text)]">{{ power.power.estimator?.confidence || '—' }}</div></div>
            </div>
            <p v-if="power.peer?.registered" class="mt-2 text-xs text-[var(--cn-text-muted)]">
              {{ t('settings.host.power_reporting', { watts: Math.round(power.peer.lastWatts), mode: power.peer.powerMode }) }}
              <span v-if="power.peer.mac" class="font-mono"> · {{ power.peer.mac }}</span>
            </p>
          </div>
          <p v-else class="text-xs text-[var(--cn-text-muted)]">
            {{ power?.error || t('settings.host.power_unreachable') }}
          </p>
        </section>

        <section class="flex flex-col gap-3 lg:col-span-2">
          <div>
            <h3 class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.host.proc_title') }}</h3>
            <p class="mt-0.5 text-xs text-[var(--cn-text-muted)]">{{ t('settings.host.proc_desc') }}</p>
          </div>
          <div class="overflow-x-auto">
            <table class="w-full text-xs">
              <thead><tr class="text-left text-[var(--cn-text-muted)]"><th class="pb-2 font-medium">PID</th><th class="pb-2 font-medium">{{ t('settings.host.proc_name') }}</th><th class="pb-2 font-medium">{{ t('settings.host.proc_owner') }}</th><th class="pb-2 text-right font-medium">CPU</th><th class="pb-2 text-right font-medium">{{ t('settings.host.proc_mem') }}</th></tr></thead>
              <tbody>
                <tr v-for="p in procs" :key="p.pid" class="border-t border-[var(--cn-border)]">
                  <td class="py-1 font-mono text-[var(--cn-text-muted)]">{{ p.pid }}</td>
                  <td class="py-1 text-[var(--cn-text)]">{{ p.name }}</td>
                  <td class="py-1 text-[var(--cn-text-muted)]">{{ p.owner || p.user }}</td>
                  <td class="py-1 text-right font-mono text-[var(--cn-text-muted)]">{{ p.cpuPct.toFixed(0) }}%</td>
                  <td class="py-1 text-right font-mono text-[var(--cn-text)]">{{ fmtBytes(p.memRssBytes) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>
      </div>
    </SettingsCard>

    <!-- ═══ Advanced: sysctl ═══ -->
    <SettingsCard :title="t('settings.host.sysctl_title')" :description="t('settings.host.sysctl_desc')">
      <template #actions>
        <Button size="small" severity="secondary" text @click="showSysctl = !showSysctl">{{ showSysctl ? t('settings.host.sysctl_hide') : t('settings.host.sysctl_show') }}</Button>
      </template>
      <div v-if="showSysctl" class="flex flex-col gap-2">
        <div v-for="sc in sysctls" :key="sc.key" class="flex items-center gap-2 text-xs">
          <span class="w-56 shrink-0 font-mono text-[var(--cn-text-muted)]">{{ sc.label }}</span>
          <InputText v-model="sysctlEdits[sc.key]" class="w-32 font-mono" />
          <Button size="small" severity="secondary" text :disabled="sysctlEdits[sc.key] === sc.value" @click="saveSysctl(sc.key)">{{ t('common.save') }}</Button>
          <span class="text-[var(--cn-text-muted)]">{{ t('settings.host.sysctl_range', { min: sc.min, max: sc.max, def: sc.default }) }}</span>
        </div>
      </div>
    </SettingsCard>

    <!-- ═══ Dialogs ═══ -->

    <Dialog :visible="!!svcConfirm" modal :header="t('settings.host.svc_confirm_title')" :style="{ width: '28rem' }" @update:visible="(v: boolean) => { if (!v) svcConfirm = null }">
      <div v-if="svcConfirm" class="flex flex-col gap-2 text-sm">
        <p class="text-[var(--cn-text-muted)]">{{ t('settings.host.svc_confirm_body', { action: svcConfirm.action, name: svcConfirm.svc.label }) }}</p>
        <p v-if="svcConfirm.svc.warn" class="rounded-lg bg-amber-500/10 px-3 py-2 text-xs text-amber-500">{{ svcConfirm.svc.warn }}</p>
      </div>
      <template #footer>
        <Button severity="secondary" text @click="svcConfirm = null">{{ t('common.cancel') }}</Button>
        <Button v-if="svcConfirm" :severity="svcConfirm.action === 'disable' ? 'danger' : 'warn'" @click="doServiceAction(svcConfirm.svc.key, svcConfirm.action)">{{ t('common.confirm') }}</Button>
      </template>
    </Dialog>
  </SettingsPage>
</template>
