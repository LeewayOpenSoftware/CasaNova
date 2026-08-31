<script setup lang="ts">
// Virtual Machines — real management UI wired to the live CasaNova-Virt
// backend (libvirt/QEMU on the host). When the host can't virtualize (or the
// virt service is down) it falls back to a capability check so the page still
// explains WHY there are no VMs instead of showing a broken list.

import { ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import AppIcon from '@/components/ui/AppIcon.vue'
import StatusChip from '@/components/ui/StatusChip.vue'
import Button from 'primevue/button'
import { useVirtStore } from '@/stores/virt'
import { systemApi } from '@/api'
import { safeCall } from '@/api/safeCall'
import type { VirtCapabilities } from '@/api/system'
import type { DomainSummary, DomainAction } from '@/api/virt'
import { getConsole } from '@/api/virt'
import VncConsole from '@/components/vms/VncConsole.vue'
import CreateVmWizard from '@/components/vms/CreateVmWizard.vue'
import VmLibrarySidebar from '@/components/vms/VmLibrarySidebar.vue'

const { t } = useI18n()
const router = useRouter()
const virt = useVirtStore()
const createVisible = ref(false)

// Capability fallback (only fetched if the backend is unavailable).
const caps = ref<VirtCapabilities | null>(null)

onMounted(async () => {
  await virt.fetchAll()
  if (virt.available === false) {
    const result = await safeCall(
      () => systemApi.getVirtCapabilities(),
      () => ({ cpu_virt: false, cpu_virt_type: '', kvm_available: false, libvirt_installed: false }),
    )
    caps.value = result.data
  } else {
    virt.startPolling()
  }
})
onBeforeUnmount(() => virt.stopPolling())

function goBack() {
  if (window.history.length > 1) router.back()
  else router.push('/app')
}

// ── status → visual + allowed actions ────────────────────────────────
const RUNNING = new Set(['running'])
const PAUSED = new Set(['paused', 'pmsuspended'])
const STARTABLE = new Set(['shutoff', 'crashed', 'nostate', 'shutdown'])

function statusClass(s: string): string {
  if (RUNNING.has(s)) return 'bg-emerald-500/15 text-emerald-500'
  if (PAUSED.has(s)) return 'bg-amber-500/15 text-amber-500'
  return 'bg-[var(--cn-surface-hover)] text-[var(--cn-text-muted)]'
}
function statusDot(s: string): string {
  if (RUNNING.has(s)) return 'bg-emerald-500'
  if (PAUSED.has(s)) return 'bg-amber-500'
  return 'bg-slate-500'
}

function fmtMem(mib: number): string {
  if (mib >= 1024) return `${(mib / 1024).toFixed(mib % 1024 ? 1 : 0)} GiB`
  return `${mib} MiB`
}
function fmtBytes(n: number): string {
  if (n >= 1024 ** 4) return `${(n / 1024 ** 4).toFixed(2)} TiB`
  if (n >= 1024 ** 3) return `${(n / 1024 ** 3).toFixed(1)} GiB`
  if (n >= 1024 ** 2) return `${(n / 1024 ** 2).toFixed(0)} MiB`
  return `${n} B`
}

async function onAction(vm: DomainSummary, act: DomainAction) {
  await virt.action(vm.name, act)
}

async function onDelete(vm: DomainSummary) {
  if (!window.confirm(t('vms.delete_confirm', { name: vm.name }))) return
  await virt.remove(vm.name)
}

// ── console (noVNC over the gateway websockify bridge) ────────────────
const consoleVisible = ref(false)
const consoleVm = ref('')
const consolePassword = ref('')
async function onConsole(vm: DomainSummary) {
  consoleVm.value = vm.name
  consolePassword.value = ''
  consoleVisible.value = true
  // Fetch the console coords only to pick up a VNC password if the VM sets
  // one — the actual pixels flow over the WebSocket that VncConsole opens.
  try {
    const info = await getConsole(vm.name)
    consolePassword.value = typeof info.password === 'string' ? info.password : ''
  } catch { /* no password / not running — VncConsole surfaces the error */ }
}

const totalMemGiB = computed(() =>
  virt.info ? (virt.info.memory_total / 1024).toFixed(1) : '—',
)
</script>

<template>
  <div class="mx-auto flex min-h-full max-w-7xl flex-col p-6">
    <!-- Header -->
    <div class="mb-6 flex flex-wrap items-center justify-between gap-4">
      <div class="flex items-center gap-4">
        <div class="flex h-14 w-14 items-center justify-center rounded-2xl border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] text-blue-500">
          <AppIcon name="Server" :size="28" />
        </div>
        <div>
          <h1 class="text-2xl font-semibold text-[var(--cn-text)]">{{ t('vms.title') }}</h1>
          <p v-if="virt.info" class="mt-1 text-sm text-[var(--cn-text-muted)]">
            {{ virt.info.hypervisor }} · libvirt {{ virt.info.libvirt_version }} ·
            {{ virt.info.cpu_count }} vCPU · {{ totalMemGiB }} GiB ·
            <span :class="virt.info.kvm_enabled ? 'text-emerald-500' : 'text-amber-500'">
              {{ virt.info.kvm_enabled ? 'KVM' : t('vms.no_kvm_accel') }}
            </span>
          </p>
          <p v-else class="mt-1 text-sm text-[var(--cn-text-muted)]">{{ t('vms.subtitle') }}</p>
        </div>
      </div>
      <div v-if="virt.available" class="flex items-center gap-2">
        <span class="text-sm text-[var(--cn-text-muted)]">
          {{ t('vms.running_of', { running: virt.running, total: virt.domains.length }) }}
        </span>
        <Button size="small" :label="t('vms.create.button')" @click="createVisible = true">
          <template #icon><AppIcon name="Plus" :size="15" /></template>
        </Button>
        <Button severity="secondary" size="small" :loading="virt.loading" @click="virt.refresh()">
          <template #icon><AppIcon name="RefreshCw" :size="15" /></template>
        </Button>
      </div>
    </div>

    <!-- ═══ REAL UI (backend available) ═══ -->
    <template v-if="virt.available">
      <!-- Two columns: the library of things VMs are built FROM on the left,
           the VMs themselves on the right. Below lg the sidebar drops under
           the list rather than squeezing both — a 72-unit column and a VM row
           do not both fit on a phone. -->
      <div class="flex flex-col gap-4 lg:flex-row lg:items-start">
        <VmLibrarySidebar class="order-2 lg:order-1" />

        <div class="order-1 min-w-0 flex-1 lg:order-2">
      <!-- VM list -->
      <div v-if="virt.domains.length === 0" class="cn-widget flex flex-col items-center gap-3 p-10 text-center">
        <AppIcon name="Server" :size="36" class="text-[var(--cn-text-muted)]" />
        <p class="text-sm text-[var(--cn-text-muted)]">{{ t('vms.no_vms') }}</p>
      </div>

      <div v-else class="flex flex-col gap-3">
        <div
          v-for="vm in virt.domains"
          :key="vm.uuid"
          class="cn-widget flex flex-wrap items-center gap-4 p-4"
        >
          <!-- identity -->
          <div class="flex min-w-0 flex-1 items-center gap-3">
            <span class="h-2.5 w-2.5 shrink-0 rounded-full" :class="statusDot(vm.status)" />
            <div class="min-w-0">
              <div class="truncate text-sm font-semibold text-[var(--cn-text)]">{{ vm.name }}</div>
              <div class="mt-0.5 flex items-center gap-2 text-xs text-[var(--cn-text-muted)]">
                <span class="rounded px-1.5 py-0.5 font-medium" :class="statusClass(vm.status)">
                  {{ t(`vms.status.${vm.status}`) }}
                </span>
                <span>{{ vm.vcpu }} vCPU</span>
                <span>·</span>
                <span>{{ fmtMem(vm.memory) }}</span>
                <span v-if="vm.autostart" class="text-blue-500">· {{ t('vms.autostart') }}</span>
              </div>
            </div>
          </div>

          <!-- lifecycle actions -->
          <div class="flex shrink-0 flex-wrap items-center justify-end gap-1.5">
            <Button
              v-if="STARTABLE.has(vm.status)"
              size="small" severity="success" :loading="virt.isBusy(vm.name)"
              :label="t('vms.action.start')" @click="onAction(vm, 'start')"
            >
              <template #icon><AppIcon name="Play" :size="14" /></template>
            </Button>
            <Button
              v-if="PAUSED.has(vm.status)"
              size="small" severity="success" :loading="virt.isBusy(vm.name)"
              :label="t('vms.action.resume')" @click="onAction(vm, 'resume')"
            >
              <template #icon><AppIcon name="Play" :size="14" /></template>
            </Button>
            <Button
              v-if="RUNNING.has(vm.status)"
              size="small" severity="secondary" :loading="virt.isBusy(vm.name)"
              :label="t('vms.action.console')" @click="onConsole(vm)"
            >
              <template #icon><AppIcon name="MonitorPlay" :size="14" /></template>
            </Button>
            <Button
              v-if="RUNNING.has(vm.status)"
              size="small" severity="secondary" :loading="virt.isBusy(vm.name)"
              :label="t('vms.action.reboot')" @click="onAction(vm, 'reboot')"
            >
              <template #icon><AppIcon name="RotateCw" :size="14" /></template>
            </Button>
            <Button
              v-if="RUNNING.has(vm.status)"
              size="small" severity="secondary" :loading="virt.isBusy(vm.name)"
              :label="t('vms.action.suspend')" @click="onAction(vm, 'suspend')"
            >
              <template #icon><AppIcon name="Pause" :size="14" /></template>
            </Button>
            <Button
              v-if="RUNNING.has(vm.status)"
              size="small" severity="warn" :loading="virt.isBusy(vm.name)"
              :label="t('vms.action.shutdown')" @click="onAction(vm, 'shutdown')"
            >
              <template #icon><AppIcon name="Power" :size="14" /></template>
            </Button>
            <Button
              v-if="RUNNING.has(vm.status) || PAUSED.has(vm.status)"
              size="small" severity="danger" :loading="virt.isBusy(vm.name)"
              :label="t('vms.action.stop')" :title="t('vms.action.stop_hint')"
              @click="onAction(vm, 'stop')"
            >
              <template #icon><AppIcon name="Square" :size="14" /></template>
            </Button>
            <Button
              size="small" severity="secondary" text
              :loading="virt.isBusy(vm.name)"
              :title="t('vms.action.delete')" @click="onDelete(vm)"
            >
              <template #icon><AppIcon name="Trash2" :size="15" class="text-red-500" /></template>
            </Button>
          </div>
        </div>
      </div>

      <!-- Storage pools -->
      <div v-if="virt.pools.length" class="mt-6">
        <h2 class="mb-3 text-sm font-semibold text-[var(--cn-text)]">{{ t('vms.pools') }}</h2>
        <div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          <div v-for="p in virt.pools" :key="p.uuid" class="cn-widget p-4">
            <div class="flex items-center justify-between">
              <span class="truncate text-sm font-medium text-[var(--cn-text)]">{{ p.name }}</span>
              <StatusChip :tone="p.state === 'running' ? 'success' : 'neutral'" size="sm">{{ p.state }}</StatusChip>
            </div>
            <div class="mt-2 h-1.5 w-full overflow-hidden rounded-full bg-[var(--cn-surface-hover)]">
              <div
                class="h-full rounded-full bg-blue-500"
                :style="{ width: p.capacity ? Math.min(100, (p.allocation / p.capacity) * 100) + '%' : '0%' }"
              />
            </div>
            <div class="mt-1.5 text-xs text-[var(--cn-text-muted)]">
              {{ fmtBytes(p.allocation) }} / {{ fmtBytes(p.capacity) }}
              <span class="text-[var(--cn-text-muted)]">· {{ fmtBytes(p.available) }} {{ t('vms.free') }}</span>
            </div>
          </div>
        </div>
      </div>
        </div>
      </div>
    </template>

    <!-- ═══ FALLBACK (host can't virtualize / service down) ═══ -->
    <template v-else-if="virt.available === false">
      <div class="cn-widget mb-6 p-5">
        <div class="mb-3 flex items-center gap-2">
          <AppIcon name="AlertTriangle" :size="18" class="text-amber-500" />
          <h2 class="text-sm font-semibold text-[var(--cn-text)]">{{ t('vms.not_available') }}</h2>
        </div>
        <p class="text-sm leading-relaxed text-[var(--cn-text-muted)]">{{ t('vms.not_available_desc') }}</p>
      </div>

      <div v-if="caps" class="cn-widget mb-6 p-5">
        <h2 class="mb-4 text-sm font-semibold text-[var(--cn-text)]">{{ t('vms.capability_check') }}</h2>
        <div class="flex flex-col gap-3">
          <div class="flex items-center gap-3">
            <AppIcon :name="caps.cpu_virt ? 'CheckCircle' : 'XCircle'" :size="18" :class="caps.cpu_virt ? 'text-emerald-500' : 'text-red-500'" />
            <div>
              <div class="text-sm text-[var(--cn-text)]">{{ t('vms.cpu_virt') }}</div>
              <div class="text-xs text-[var(--cn-text-muted)]">
                {{ caps.cpu_virt ? t('vms.cpu_virt_yes', { type: caps.cpu_virt_type.toUpperCase() }) : t('vms.cpu_virt_no') }}
              </div>
            </div>
          </div>
          <div class="flex items-center gap-3">
            <AppIcon :name="caps.kvm_available ? 'CheckCircle' : 'XCircle'" :size="18" :class="caps.kvm_available ? 'text-emerald-500' : 'text-red-500'" />
            <div>
              <div class="text-sm text-[var(--cn-text)]">{{ t('vms.kvm_module') }}</div>
              <div class="text-xs text-[var(--cn-text-muted)]">{{ caps.kvm_available ? t('vms.kvm_yes') : t('vms.kvm_no') }}</div>
            </div>
          </div>
          <div class="flex items-center gap-3">
            <AppIcon :name="caps.libvirt_installed ? 'CheckCircle' : 'XCircle'" :size="18" :class="caps.libvirt_installed ? 'text-emerald-500' : 'text-amber-400'" />
            <div>
              <div class="text-sm text-[var(--cn-text)]">{{ t('vms.libvirt') }}</div>
              <div class="text-xs text-[var(--cn-text-muted)]">{{ caps.libvirt_installed ? t('vms.libvirt_yes') : t('vms.libvirt_no') }}</div>
            </div>
          </div>
        </div>
      </div>

      <div>
        <Button severity="secondary" @click="goBack">{{ t('not_implemented.back') }}</Button>
      </div>
    </template>

    <!-- Console — a self-contained draggable floating window (own toolbar +
         drag handle + keyboard focus for the guest). Not a PrimeVue modal:
         that trapped focus (no typing into the VM) and stacked a 2nd title. -->
    <VncConsole
      v-if="consoleVisible" :key="consoleVm"
      :name="consoleVm" :password="consolePassword"
      @close="consoleVisible = false"
    />

    <!-- Create-VM wizard -->
    <CreateVmWizard v-model:visible="createVisible" @created="virt.fetchAll()" />
  </div>
</template>
