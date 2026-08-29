<script setup lang="ts">
// PhysicalPortsCard — hardware port visualization (Ethernet / USB /
// Display) inspired by ZimaOS. Shows what the user physically sees on
// the back of the device, with live green dots on connected ports.
//
// Self-contained: fetches its own hardware snapshot so it can be
// dropped into any settings page (Network, Server, …) without the host
// view needing to wire up detection. Previously this markup lived
// inline in NetworkSettings; it's factored out here so the Server page
// can render the exact same map with zero duplication.

import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import PortGlyph from '@/components/settings/PortGlyph.vue'
import { getSystemHardware, type PhysicalPort, type MotherboardProfile } from '@/api/hardware'

const { t } = useI18n()

const ports = ref<PhysicalPort[]>([])
const boardProfile = ref<MotherboardProfile | null>(null)

// Group ports by kind for the template sections.
const ethernetPorts = computed(() => ports.value.filter((p) => p.kind === 'ethernet' && p.location === 'rear'))
const usbPorts = computed(() => ports.value.filter((p) => (p.kind === 'usb3' || p.kind === 'usb2' || p.kind === 'usbc') && p.location === 'rear'))
const displayPorts = computed(() => ports.value.filter((p) => p.kind === 'hdmi' || p.kind === 'vga' || p.kind === 'displayport'))

onMounted(async () => {
  try {
    const hw = await getSystemHardware()
    ports.value = hw.ports
    boardProfile.value = hw.motherboard
  } catch { /* fallback: no ports shown */ }
})
</script>

<template>
  <SettingsCard
    :title="t('settings.network.ports')"
    :description="t('settings.network.ports_desc')"
  >
    <div class="flex flex-wrap items-start gap-x-8 gap-y-6">
      <!-- Ethernet block -->
      <div>
        <div class="mb-3 text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">
          {{ t('settings.network.ethernet') }}
        </div>
        <div class="flex gap-4">
          <div v-for="p in ethernetPorts" :key="p.id" class="flex flex-col items-center gap-1">
            <PortGlyph kind="ethernet" :connected="p.connected" />
            <span class="font-mono text-[10px] text-[var(--cn-text-muted)]">{{ p.label }}</span>
          </div>
        </div>
      </div>

      <!-- USB block -->
      <div>
        <div class="mb-3 text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">
          {{ t('settings.network.usb') }}
        </div>
        <div class="flex gap-3">
          <div v-for="p in usbPorts" :key="p.id" class="flex flex-col items-center gap-1">
            <PortGlyph :kind="p.kind" :connected="p.connected" />
          </div>
        </div>
      </div>

      <!-- Display block -->
      <div>
        <div class="mb-3 text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">
          {{ t('settings.network.display') }}
        </div>
        <div class="flex gap-3">
          <div v-for="p in displayPorts" :key="p.id" class="flex flex-col items-center gap-1">
            <PortGlyph :kind="p.kind" :connected="p.connected" />
            <span class="text-[10px] text-[var(--cn-text-muted)]">
              {{ p.kind === 'hdmi' ? 'HDMI' : p.kind === 'vga' ? 'VGA' : 'DP' }}
            </span>
          </div>
        </div>
      </div>
    </div>

    <!-- Board identification — shows when a known profile is detected -->
    <div
      v-if="boardProfile"
      class="mt-4 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-3 py-2 text-xs text-[var(--cn-text-muted)]"
    >
      <span class="font-medium text-[var(--cn-text)]">{{ boardProfile.manufacturer }} {{ boardProfile.model }}</span>
      · {{ boardProfile.chipset }} · {{ boardProfile.formFactor }} · Socket {{ boardProfile.socket }}
    </div>

    <!-- Connection details — one row per NIC -->
    <div v-if="ethernetPorts.length > 0" class="mt-5 border-t border-[var(--cn-border)] pt-4">
      <div class="mb-3 text-xs font-medium text-[var(--cn-text-muted)]">
        {{ t('settings.network.connection') }}
      </div>
      <div class="flex flex-col gap-2">
        <div
          v-for="p in ethernetPorts"
          :key="p.id"
          class="flex items-center justify-between gap-3 rounded-lg border border-[var(--cn-border)] px-3 py-2"
        >
          <div class="flex min-w-0 items-center gap-3">
            <span
              class="h-1.5 w-1.5 shrink-0 rounded-full"
              :class="p.connected
                ? 'bg-emerald-500 shadow-[0_0_6px_rgba(16,185,129,0.6)]'
                : 'bg-[var(--cn-border)]'"
            />
            <span class="font-mono text-xs text-[var(--cn-text)]">{{ p.label }}</span>
            <span v-if="p.deviceName" class="truncate text-xs text-[var(--cn-text-muted)]">
              {{ p.deviceName }}
            </span>
          </div>
          <div class="flex shrink-0 items-center gap-2 text-xs">
            <span class="rounded-md border border-[var(--cn-border)] px-2 py-0.5 text-[var(--cn-text-muted)]">
              LAN
            </span>
            <span v-if="p.speed" class="rounded-md border border-[var(--cn-border)] px-2 py-0.5 text-[var(--cn-text)]">
              {{ p.speed }}
            </span>
          </div>
        </div>
      </div>
    </div>

    <!-- USB device detail — only if something is actually plugged in -->
    <div
      v-if="usbPorts.some((p) => p.connected)"
      class="mt-3 flex flex-wrap gap-2"
    >
      <div
        v-for="p in usbPorts.filter((x) => x.connected)"
        :key="p.id"
        class="inline-flex items-center gap-2 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-3 py-1.5 text-xs"
      >
        <AppIcon name="Usb" :size="12" class="text-amber-500" />
        <span class="text-[var(--cn-text-muted)]">{{ p.label }}:</span>
        <span class="text-[var(--cn-text)]">{{ p.deviceName }}</span>
      </div>
    </div>
  </SettingsCard>
</template>
