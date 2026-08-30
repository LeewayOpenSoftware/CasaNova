<script setup lang="ts">
// ServerSettings — "About the server" page.
//
// Shows the server hardware image, real-time system info pulled from
// /v1/sys/hardware and /v1/sys/utilization, and power actions
// (reboot / shutdown) with confirmation dialogs.

import { onMounted, onBeforeUnmount, ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import Button from 'primevue/button'
import Dialog from 'primevue/dialog'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import PhysicalPortsCard from '@/components/settings/PhysicalPortsCard.vue'
import SystemStatsPanel from '@/components/dashboard/SystemStatsPanel.vue'
import { useSystemStore } from '@/stores/system'
import { systemApi } from '@/api'
import { getSystemHardware, type MotherboardProfile } from '@/api/hardware'
import { safeCall } from '@/api/safeCall'

const { t } = useI18n()

// ─── Hardware info (static, loaded once) ────────────────────────────────────
const hwCpu = ref('—')
const hwCores = ref(0)
const hwArch = ref('—')
const hwMemory = ref('—')
const hwHostname = ref('—')
const hwKernel = ref('—')
// Display-safe server model (BIOS placeholders already filtered out by
// the hardware API — empty string means "nothing worth showing").
const hwModel = ref('')

// ─── Motherboard (board_name + vendor + chipset/socket when known) ──────────
const boardProfile = ref<MotherboardProfile | null>(null)
const boardName = ref('')
const boardVendor = ref('')

// ─── Live utilization (polled every 3s) ─────────────────────────────────────
const cpuPercent = ref(0)
const cpuTemp = ref(0)
const memUsed = ref(0)
const memTotal = ref(0)
const memPercent = ref(0)
const diskUsed = ref(0)
const diskTotal = ref(0)
const uptime = ref(0)

let pollTimer: ReturnType<typeof setInterval> | null = null

// ─── Power dialogs ──────────────────────────────────────────────────────────
const rebootDialogVisible = ref(false)
const shutdownDialogVisible = ref(false)
const powerActionLoading = ref(false)

// ─── Formatters ─────────────────────────────────────────────────────────────
function formatBytes(n: number): string {
  if (n >= 1024 ** 4) return (n / 1024 ** 4).toFixed(1) + ' TB'
  if (n >= 1024 ** 3) return (n / 1024 ** 3).toFixed(1) + ' GB'
  if (n >= 1024 ** 2) return (n / 1024 ** 2).toFixed(1) + ' MB'
  return (n / 1024).toFixed(0) + ' KB'
}

function formatUptime(seconds: number): string {
  if (seconds <= 0) return '—'
  const d = Math.floor(seconds / 86400)
  const h = Math.floor((seconds % 86400) / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const parts: string[] = []
  if (d > 0) parts.push(`${d}d`)
  if (h > 0) parts.push(`${h}h`)
  parts.push(`${m}m`)
  return parts.join(' ')
}

const diskPercent = computed(() => {
  if (diskTotal.value === 0) return 0
  return Math.round((diskUsed.value / diskTotal.value) * 100)
})

// Motherboard summary line. Prefers the rich known-board profile
// (vendor · chipset · form factor · socket); otherwise composes what
// DMI gives us (vendor + board_name). Empty when nothing is known.
const motherboardLabel = computed(() => {
  if (boardProfile.value) {
    const p = boardProfile.value
    const bits = [
      `${p.manufacturer} ${p.model}`.trim(),
      p.chipset,
      p.formFactor,
      p.socket ? `Socket ${p.socket}` : '',
    ].filter(Boolean)
    return bits.join(' · ')
  }
  return `${boardVendor.value} ${boardName.value}`.trim()
})

// ─── Data loading ───────────────────────────────────────────────────────────
// Uses the hardware API (getSystemHardware) rather than the raw
// /v1/sys/hardware wrapper: the endpoint returns FLAT snake_case fields
// (cpu_model, board_name) and carries no core count — cores come from
// /v1/sys/utilization.cpu.num. getSystemHardware merges both sources and
// filters BIOS placeholders, so model/cores/motherboard resolve correctly.
async function loadHardware() {
  try {
    const hw = await getSystemHardware()
    hwCpu.value = hw.cpu.model || '—'
    hwCores.value = hw.cpu.cores || 0
    hwArch.value = hw.cpu.architecture || '—'
    hwHostname.value = hw.hostname || '—'
    hwKernel.value = hw.kernelVersion || '—'
    hwModel.value = hw.model || ''
    boardProfile.value = hw.motherboard
    boardName.value = hw.boardName || ''
    boardVendor.value = hw.boardVendor || ''
    if (hw.totalMemoryBytes) {
      hwMemory.value = formatBytes(hw.totalMemoryBytes)
    }
  } catch { /* leave placeholders — the utilization poll still populates live stats */ }
}

async function pollUtilization() {
  const res = await safeCall(() => systemApi.getUtilization())
  if (res.data) {
    cpuPercent.value = Math.round(res.data.cpu.percent)
    cpuTemp.value = Math.round(res.data.cpu.temp)
    memUsed.value = res.data.mem.used
    memTotal.value = res.data.mem.total
    memPercent.value = Math.round(res.data.mem.usedPercent)
    diskUsed.value = res.data.sysDisk.used
    diskTotal.value = res.data.sysDisk.size
    uptime.value = res.data.uptime
  }
}

// Live history buffer for the sparkline panel. `startPolling` is
// idempotent — the dashboard route may already have started it. We
// kick it again here for the case where Server settings is the first
// page the user lands on after login.
const system = useSystemStore()

onMounted(async () => {
  await loadHardware()
  await pollUtilization()
  pollTimer = setInterval(pollUtilization, 3000)
  system.startPolling()
})

onBeforeUnmount(() => {
  if (pollTimer) clearInterval(pollTimer)
  // Release our reference to the shared poll. The store refcounts, so any
  // other consumer still mounted keeps it running; if we are the last one it
  // actually stops, instead of polling this page's utilization endpoint for
  // the rest of the session from a view nobody is looking at.
  system.stopPolling()
})

// ─── Power actions ──────────────────────────────────────────────────────────
async function confirmReboot() {
  powerActionLoading.value = true
  try {
    await systemApi.reboot()
  } finally {
    powerActionLoading.value = false
    rebootDialogVisible.value = false
  }
}

async function confirmShutdown() {
  powerActionLoading.value = true
  try {
    await systemApi.shutdown()
  } finally {
    powerActionLoading.value = false
    shutdownDialogVisible.value = false
  }
}
</script>

<template>
  <SettingsPage
    icon-src="/assets/brand/server.png"
    :title="t('settings.server.title')"
    :description="t('settings.server.description')"
  >
    <!-- Hero row: identity on the left, power actions beside it.
         Reboot and shutdown used to sit in the last card on the page,
         below the stats and the port map — several screens of scrolling
         away from the only thing an operator opening this page in a
         hurry wants. They are short controls and the identity card runs
         out of text well before it runs out of width, so the two share a
         row: the hero gives up the space it was not using, and the
         actions land in the first screenful.

         Below lg the column collapses and they stack in the same order,
         which keeps the actions above the fold on a phone too. -->
    <div class="grid grid-cols-1 gap-4 lg:grid-cols-[minmax(0,1fr)_20rem]">
      <!-- Identity: server image + hostname/model/uptime/board -->
      <SettingsCard :title="t('settings.server.identity')">
        <div class="flex flex-col items-center gap-5 sm:flex-row sm:items-start">
          <!-- Server image -->
          <div class="flex w-32 shrink-0 items-center justify-center sm:w-40">
            <img
              src="/assets/brand/server.png"
              alt="Server"
              class="h-auto w-full drop-shadow-lg"
            />
          </div>
          <!-- Identity info -->
          <div class="flex flex-1 flex-col gap-3">
            <div>
              <div class="text-2xl font-bold text-[var(--cn-text)]">{{ hwHostname }}</div>
              <!-- Model — resolved from product_name, falling back to the
                   motherboard when the BIOS ships a placeholder. Hidden
                   when there is genuinely nothing to show. -->
              <div v-if="hwModel" class="mt-0.5 text-sm font-medium text-[var(--cn-text-muted)]">
                {{ hwModel }}
              </div>
              <div class="mt-0.5 text-xs text-[var(--cn-text-muted)]">
                {{ hwArch }} &middot; {{ hwKernel }}
              </div>
            </div>
            <div class="flex items-center gap-2 text-sm text-[var(--cn-text)]">
              <AppIcon name="Clock" :size="14" class="text-[var(--cn-text-muted)]" />
              <span>{{ t('settings.server.uptime') }}:</span>
              <span class="font-mono font-medium">{{ formatUptime(uptime) }}</span>
            </div>
            <!-- Motherboard — surfaced here (not only on the Network page)
                 so the About/Server page shows the physical board. -->
            <div v-if="motherboardLabel" class="flex items-center gap-2 text-sm text-[var(--cn-text)]">
              <AppIcon name="CircuitBoard" :size="14" class="text-[var(--cn-text-muted)]" />
              <span>{{ t('settings.server.motherboard') }}:</span>
              <span class="font-medium">{{ motherboardLabel }}</span>
            </div>
          </div>
        </div>
      </SettingsCard>

      <!-- Power actions, now beside the identity rather than below the
           whole page. -->
      <SettingsCard
        :title="t('settings.server.power')"
        :description="t('settings.server.power_desc')"
      >
        <div class="flex flex-wrap gap-3">
          <Button
            severity="warn"
            :label="t('settings.server.reboot')"
            @click="rebootDialogVisible = true"
          >
            <template #icon><AppIcon name="RotateCcw" :size="16" /></template>
          </Button>
          <Button
            severity="danger"
            :label="t('settings.server.shutdown')"
            @click="shutdownDialogVisible = true"
          >
            <template #icon><AppIcon name="Power" :size="16" /></template>
          </Button>
        </div>
      </SettingsCard>
    </div>

    <!-- System info grid -->
    <SettingsCard :title="t('settings.server.system_info')" :description="t('settings.server.system_info_desc')">
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
        <!-- CPU -->
        <div class="flex items-start gap-3 rounded-lg border border-[var(--cn-border)] p-4">
          <div class="mt-0.5 flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-blue-500/10">
            <AppIcon name="Cpu" :size="18" class="text-blue-500" />
          </div>
          <div class="min-w-0 flex-1">
            <div class="text-[10px] uppercase tracking-wide text-[var(--cn-text-muted)]">CPU</div>
            <div class="mt-0.5 truncate text-sm font-medium text-[var(--cn-text)]">{{ hwCpu }}</div>
            <!-- Only show the core count when we actually know it. A real
                 0 never happens; 0 means "unknown", so hide the line
                 rather than claiming the CPU has zero cores. -->
            <div v-if="hwCores > 0" class="mt-1 text-xs text-[var(--cn-text-muted)]">
              {{ hwCores }} {{ t('settings.server.cores') }}
            </div>
            <div class="mt-2 flex items-center gap-3">
              <div class="flex-1">
                <div class="h-1.5 w-full overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
                  <div
                    class="h-full rounded-full transition-all"
                    :class="cpuPercent > 85 ? 'bg-red-500' : cpuPercent > 60 ? 'bg-amber-500' : 'bg-blue-500'"
                    :style="{ width: cpuPercent + '%' }"
                  />
                </div>
              </div>
              <span class="shrink-0 font-mono text-xs font-medium text-[var(--cn-text)]">{{ cpuPercent }}%</span>
            </div>
            <div v-if="cpuTemp > 0" class="mt-1 flex items-center gap-1 text-xs text-[var(--cn-text-muted)]">
              <AppIcon name="Thermometer" :size="12" />
              <span>{{ cpuTemp }}°C</span>
            </div>
          </div>
        </div>

        <!-- Memory -->
        <div class="flex items-start gap-3 rounded-lg border border-[var(--cn-border)] p-4">
          <div class="mt-0.5 flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-emerald-500/10">
            <AppIcon name="MemoryStick" :size="18" class="text-emerald-500" />
          </div>
          <div class="min-w-0 flex-1">
            <div class="text-[10px] uppercase tracking-wide text-[var(--cn-text-muted)]">{{ t('settings.server.memory') }}</div>
            <div class="mt-0.5 text-sm font-medium text-[var(--cn-text)]">{{ hwMemory }}</div>
            <div class="mt-1 text-xs text-[var(--cn-text-muted)]">
              {{ formatBytes(memUsed) }} {{ t('settings.server.used_of') }} {{ formatBytes(memTotal) }}
            </div>
            <div class="mt-2 flex items-center gap-3">
              <div class="flex-1">
                <div class="h-1.5 w-full overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
                  <div
                    class="h-full rounded-full transition-all"
                    :class="memPercent > 85 ? 'bg-red-500' : memPercent > 60 ? 'bg-amber-500' : 'bg-emerald-500'"
                    :style="{ width: memPercent + '%' }"
                  />
                </div>
              </div>
              <span class="shrink-0 font-mono text-xs font-medium text-[var(--cn-text)]">{{ memPercent }}%</span>
            </div>
          </div>
        </div>

        <!-- System Disk -->
        <div class="flex items-start gap-3 rounded-lg border border-[var(--cn-border)] p-4">
          <div class="mt-0.5 flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-purple-500/10">
            <AppIcon name="HardDrive" :size="18" class="text-purple-500" />
          </div>
          <div class="min-w-0 flex-1">
            <div class="text-[10px] uppercase tracking-wide text-[var(--cn-text-muted)]">{{ t('settings.server.system_disk') }}</div>
            <div class="mt-0.5 text-sm font-medium text-[var(--cn-text)]">{{ formatBytes(diskTotal) }}</div>
            <div class="mt-1 text-xs text-[var(--cn-text-muted)]">
              {{ formatBytes(diskUsed) }} {{ t('settings.server.used_of') }} {{ formatBytes(diskTotal) }}
            </div>
            <div class="mt-2 flex items-center gap-3">
              <div class="flex-1">
                <div class="h-1.5 w-full overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
                  <div
                    class="h-full rounded-full transition-all"
                    :class="diskPercent > 85 ? 'bg-red-500' : diskPercent > 60 ? 'bg-amber-500' : 'bg-purple-500'"
                    :style="{ width: diskPercent + '%' }"
                  />
                </div>
              </div>
              <span class="shrink-0 font-mono text-xs font-medium text-[var(--cn-text)]">{{ diskPercent }}%</span>
            </div>
          </div>
        </div>
      </div>
    </SettingsCard>

    <!-- Physical ports — same hardware port map as the Network page,
         so the operator can see the back-panel layout and live link
         state from the Server section too. -->
    <PhysicalPortsCard />

    <!-- Live history graphs — Zima-style 2x2 sparklines for CPU /
         memory / network up / down. SystemStatsPanel already wraps
         itself in a card-style container, so dropping it in here
         keeps the visual rhythm consistent with the other sections. -->
    <SystemStatsPanel />

  </SettingsPage>

  <!-- Reboot confirmation dialog -->
  <Dialog
    v-model:visible="rebootDialogVisible"
    :header="t('settings.server.reboot_confirm_title')"
    modal
    :style="{ width: '24rem' }"
    :closable="true"
  >
    <div class="flex flex-col items-center gap-4 py-2">
      <div class="flex h-14 w-14 items-center justify-center rounded-full bg-amber-500/10">
        <AppIcon name="RotateCcw" :size="28" class="text-amber-500" />
      </div>
      <p class="text-center text-sm text-[var(--cn-text-muted)]">
        {{ t('settings.server.reboot_confirm_body') }}
      </p>
    </div>
    <template #footer>
      <div class="flex justify-end gap-2">
        <Button size="small" severity="secondary" @click="rebootDialogVisible = false">
          {{ t('common.cancel') }}
        </Button>
        <Button
          size="small"
          severity="warn"
          :loading="powerActionLoading"
          :label="t('settings.server.reboot')"
          @click="confirmReboot"
        >
          <template #icon><AppIcon name="RotateCcw" :size="13" /></template>
        </Button>
      </div>
    </template>
  </Dialog>

  <!-- Shutdown confirmation dialog -->
  <Dialog
    v-model:visible="shutdownDialogVisible"
    :header="t('settings.server.shutdown_confirm_title')"
    modal
    :style="{ width: '24rem' }"
    :closable="true"
  >
    <div class="flex flex-col items-center gap-4 py-2">
      <div class="flex h-14 w-14 items-center justify-center rounded-full bg-red-500/10">
        <AppIcon name="Power" :size="28" class="text-red-500" />
      </div>
      <p class="text-center text-sm text-[var(--cn-text-muted)]">
        {{ t('settings.server.shutdown_confirm_body') }}
      </p>
    </div>
    <template #footer>
      <div class="flex justify-end gap-2">
        <Button size="small" severity="secondary" @click="shutdownDialogVisible = false">
          {{ t('common.cancel') }}
        </Button>
        <Button
          size="small"
          severity="danger"
          :loading="powerActionLoading"
          :label="t('settings.server.shutdown')"
          @click="confirmShutdown"
        >
          <template #icon><AppIcon name="Power" :size="13" /></template>
        </Button>
      </div>
    </template>
  </Dialog>
</template>
