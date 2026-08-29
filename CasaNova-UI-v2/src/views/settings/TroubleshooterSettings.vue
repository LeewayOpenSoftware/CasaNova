<script setup lang="ts">
import AppIcon from '@/components/ui/AppIcon.vue'
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Button from 'primevue/button'
import Dialog from 'primevue/dialog'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { runTroubleshoot, type TroubleshootAction } from '@/api/system'

const { t } = useI18n()

interface TroubleshootItem {
  action: TroubleshootAction
  labelKey: string
  descKey: string
  confirmKey?: string
  icon: string
  severity: 'info' | 'warn' | 'danger'
}

const actions: TroubleshootItem[] = [
  {
    action: 'fix-docker-network',
    labelKey: 'settings.troubleshooter.fix_docker_network',
    descKey: 'settings.troubleshooter.fix_docker_network_detail',
    confirmKey: 'settings.troubleshooter.confirm_docker',
    icon: 'Wifi',
    severity: 'warn',
  },
  {
    action: 'restart-gateway',
    labelKey: 'settings.troubleshooter.restart_gateway',
    descKey: 'settings.troubleshooter.restart_gateway_desc',
    icon: 'RefreshCw',
    severity: 'info',
  },
  {
    action: 'flush-dns',
    labelKey: 'settings.troubleshooter.flush_dns',
    descKey: 'settings.troubleshooter.flush_dns_desc',
    icon: 'Globe',
    severity: 'info',
  },
  {
    action: 'restart-all',
    labelKey: 'settings.troubleshooter.restart_all',
    descKey: 'settings.troubleshooter.restart_all_desc',
    confirmKey: 'settings.troubleshooter.confirm_restart_all',
    icon: 'RotateCcw',
    severity: 'danger',
  },
]

// Per-action state tracking
const running = ref<Record<string, boolean>>({})
const results = ref<Record<string, { ok: boolean; message: string }>>({})

// Confirmation dialog
const confirmVisible = ref(false)
const pendingAction = ref<TroubleshootItem | null>(null)

function requestAction(item: TroubleshootItem) {
  if (item.confirmKey) {
    pendingAction.value = item
    confirmVisible.value = true
  } else {
    executeAction(item)
  }
}

function confirmAction() {
  confirmVisible.value = false
  if (pendingAction.value) {
    executeAction(pendingAction.value)
    pendingAction.value = null
  }
}

async function executeAction(item: TroubleshootItem) {
  running.value[item.action] = true
  delete results.value[item.action]
  try {
    const msg = await runTroubleshoot(item.action)
    results.value[item.action] = { ok: true, message: msg }
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    results.value[item.action] = { ok: false, message: msg }
  } finally {
    running.value[item.action] = false
  }
}

function severityClass(severity: string) {
  if (severity === 'danger') return 'p-button-danger'
  if (severity === 'warn') return 'p-button-warn'
  return 'p-button-secondary'
}
</script>

<template>
  <SettingsPage
    icon="Wrench"
    :title="t('settings.troubleshooter.title')"
    :description="t('settings.troubleshooter.description')"
  >
    <!-- Network Repair -->
    <SettingsCard
      :title="t('settings.troubleshooter.network_repair')"
      :description="t('settings.troubleshooter.network_repair_desc')"
    >
      <div class="flex flex-col gap-4">
        <div
          v-for="item in actions.filter(a => ['fix-docker-network', 'flush-dns'].includes(a.action))"
          :key="item.action"
          class="flex items-start justify-between gap-4 rounded-lg border border-[var(--cn-border)] p-4"
        >
          <div class="flex items-start gap-3">
            <AppIcon :name="item.icon" :size="20" class="mt-0.5 shrink-0 text-[var(--cn-text-muted)]" />
            <div>
              <div class="text-sm font-medium text-[var(--cn-text)]">{{ t(item.labelKey) }}</div>
              <div class="mt-1 text-xs text-[var(--cn-text-muted)]">{{ t(item.descKey) }}</div>
              <div
                v-if="results[item.action]"
                class="mt-2 text-xs font-medium"
                :class="results[item.action].ok ? 'text-emerald-500' : 'text-red-400'"
              >
                {{ results[item.action].ok ? t('settings.troubleshooter.success') : t('settings.troubleshooter.failed') }}
                <span class="font-normal text-[var(--cn-text-muted)]">{{ results[item.action].message }}</span>
              </div>
            </div>
          </div>
          <Button
            :label="running[item.action] ? t('settings.troubleshooter.running') : t(item.labelKey)"
            :class="severityClass(item.severity)"
            :loading="running[item.action]"
            :disabled="!!running[item.action]"
            size="small"
            @click="requestAction(item)"
          />
        </div>
      </div>
    </SettingsCard>

    <!-- Service Management -->
    <SettingsCard
      :title="t('settings.troubleshooter.service_management')"
      :description="t('settings.troubleshooter.service_management_desc')"
    >
      <div class="flex flex-col gap-4">
        <div
          v-for="item in actions.filter(a => ['restart-gateway', 'restart-all'].includes(a.action))"
          :key="item.action"
          class="flex items-start justify-between gap-4 rounded-lg border border-[var(--cn-border)] p-4"
        >
          <div class="flex items-start gap-3">
            <AppIcon :name="item.icon" :size="20" class="mt-0.5 shrink-0 text-[var(--cn-text-muted)]" />
            <div>
              <div class="text-sm font-medium text-[var(--cn-text)]">{{ t(item.labelKey) }}</div>
              <div class="mt-1 text-xs text-[var(--cn-text-muted)]">{{ t(item.descKey) }}</div>
              <div
                v-if="results[item.action]"
                class="mt-2 text-xs font-medium"
                :class="results[item.action].ok ? 'text-emerald-500' : 'text-red-400'"
              >
                {{ results[item.action].ok ? t('settings.troubleshooter.success') : t('settings.troubleshooter.failed') }}
                <span class="font-normal text-[var(--cn-text-muted)]">{{ results[item.action].message }}</span>
              </div>
            </div>
          </div>
          <Button
            :label="running[item.action] ? t('settings.troubleshooter.running') : t(item.labelKey)"
            :class="severityClass(item.severity)"
            :loading="running[item.action]"
            :disabled="!!running[item.action]"
            size="small"
            @click="requestAction(item)"
          />
        </div>
      </div>
    </SettingsCard>

    <!-- Confirmation dialog -->
    <Dialog
      v-model:visible="confirmVisible"
      :header="t('settings.troubleshooter.confirm_title')"
      modal
      :style="{ width: '28rem' }"
    >
      <p class="text-sm text-[var(--cn-text-muted)]">
        {{ pendingAction?.confirmKey ? t(pendingAction.confirmKey) : '' }}
      </p>
      <template #footer>
        <Button :label="t('common.cancel')" class="p-button-text" @click="confirmVisible = false" />
        <Button
          :label="t('common.confirm')"
          :class="pendingAction?.severity === 'danger' ? 'p-button-danger' : 'p-button-warn'"
          @click="confirmAction"
        />
      </template>
    </Dialog>
  </SettingsPage>
</template>
