<script setup lang="ts">
// Virtual Machines widget — compact summary of the host's VMs, wired to
// the virt store (CasaNova-Virt / libvirt backend).
//
// `virt.available === false` means the host can't virtualize (no working
// hypervisor / service down) — we show a single "no disponible" line
// instead of an empty list. Otherwise we surface running/total counts,
// the hypervisor + KVM badge, and the first few VMs with a status dot.
//
// fetchAll() is the availability probe + initial load; the store guards
// against redundant work so calling it on mount is idempotent. We reuse
// the store's own 10s polling for freshness and stop it on unmount.

import { computed, onMounted, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useVirtStore } from '@/stores/virt'
import type { DomainSummary } from '@/api/virt'

const { t } = useI18n()
const router = useRouter()
const virt = useVirtStore()

onMounted(() => {
  virt.fetchAll()
  virt.startPolling()
})

onBeforeUnmount(() => {
  virt.stopPolling()
})

// Prefer the live info counts (authoritative on the host) but fall back
// to the domain list length so the widget still reads sensibly if info
// hasn't loaded yet.
const totalCount = computed(() => virt.info?.total_domains ?? virt.domains.length)
const runningCount = computed(() => virt.info?.active_domains ?? virt.running)

// First few VMs for the mini list. Running ones surface first so the
// most relevant machines are visible in the tight three-row budget.
const topDomains = computed<DomainSummary[]>(() =>
  [...virt.domains]
    .sort((a, b) => Number(b.status === 'running') - Number(a.status === 'running'))
    .slice(0, 3),
)

type DotState = 'running' | 'paused' | 'stopped'
function dotState(status: DomainSummary['status']): DotState {
  if (status === 'running') return 'running'
  if (status === 'paused' || status === 'pmsuspended') return 'paused'
  return 'stopped'
}

const dotClass: Record<DotState, string> = {
  running: 'bg-emerald-500',
  paused: 'bg-amber-500',
  stopped: 'bg-slate-500',
}

function openVms() {
  router.push('/app/vms')
}
</script>

<template>
  <div class="cn-widget p-4 md:p-5">
    <div class="mb-3 flex items-center justify-between">
      <button
        type="button"
        class="flex items-center gap-2 text-left"
        @click="openVms"
      >
        <AppIcon name="Server" :size="16" class="text-[var(--cn-text-muted)]" />
        <h3 class="text-sm font-semibold text-[var(--cn-text)]">
          {{ t('dashboard.widgets.vms.title') }}
        </h3>
      </button>
      <button
        type="button"
        class="text-xs text-blue-500 hover:underline"
        @click="openVms"
      >
        {{ t('dashboard.widgets.vms.view_all') }}
      </button>
    </div>

    <!-- Capability fallback — host can't virtualize / service is down. -->
    <div
      v-if="virt.available === false"
      class="flex items-center gap-2 text-xs text-[var(--cn-text-muted)]"
    >
      <AppIcon name="ServerOff" :size="14" />
      <span>{{ t('dashboard.widgets.vms.unavailable') }}</span>
    </div>

    <template v-else>
      <!-- Running / total + hypervisor & KVM badge -->
      <div class="flex items-center justify-between gap-2">
        <div class="text-sm text-[var(--cn-text)]">
          <span class="font-semibold tabular-nums text-emerald-500">{{ runningCount }}</span>
          <span class="text-[var(--cn-text-muted)]"> / {{ totalCount }} </span>
          <span class="text-xs text-[var(--cn-text-muted)]">{{ t('dashboard.widgets.vms.running') }}</span>
        </div>
        <div v-if="virt.info" class="flex items-center gap-1">
          <span class="rounded-full border border-[var(--cn-border)] px-2 py-0.5 text-[10px] font-medium text-[var(--cn-text-muted)]">
            {{ virt.info.hypervisor }}
          </span>
          <span
            v-if="virt.info.kvm_enabled"
            class="rounded-full border border-emerald-500/30 bg-emerald-500/15 px-2 py-0.5 text-[10px] font-medium text-emerald-400"
          >
            KVM
          </span>
        </div>
      </div>

      <!-- Mini VM list -->
      <ul v-if="topDomains.length > 0" class="mt-3 flex flex-col gap-1">
        <li
          v-for="d in topDomains"
          :key="d.uuid"
          class="flex items-center gap-2 text-xs"
        >
          <span
            class="h-2 w-2 shrink-0 rounded-full"
            :class="dotClass[dotState(d.status)]"
          />
          <span class="truncate text-[var(--cn-text)]">{{ d.name }}</span>
        </li>
      </ul>
      <div
        v-else
        class="mt-3 text-xs text-[var(--cn-text-muted)]"
      >
        {{ t('dashboard.widgets.vms.empty') }}
      </div>
    </template>
  </div>
</template>
