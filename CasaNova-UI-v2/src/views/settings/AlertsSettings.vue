<script setup lang="ts">
// Alerts & Thresholds settings — configure automatic alerts when system
// metrics exceed thresholds. Reads/writes via /v1/sys/alerts/* endpoints.

import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import InputText from 'primevue/inputtext'
import InputNumber from 'primevue/inputnumber'
import Select from 'primevue/select'
import ToggleSwitch from 'primevue/toggleswitch'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import dayjs from 'dayjs'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { alertsApi } from '@/api'
import { safeCall } from '@/api/safeCall'
import type { AlertRule, AlertEvent } from '@/api/alerts'

const { t } = useI18n()

// ---- State ----

const rules = ref<AlertRule[]>([])
const history = ref<AlertEvent[]>([])
const loading = ref(false)

// Dialog state
const dialogVisible = ref(false)
const editingRule = ref<AlertRule | null>(null)

// Form fields
const formMetric = ref('cpu_percent')
const formOperator = ref('gt')
const formThreshold = ref<number>(90)
const formDuration = ref<number>(300)
const formLabel = ref('')
const formEnabled = ref(true)
const formNotify = ref('dashboard')

// Delete confirm
const deleteDialogVisible = ref(false)
const deletingRule = ref<AlertRule | null>(null)

// ---- Options ----

const metricOptions = computed(() => [
  { code: 'cpu_percent', label: t('settings.alerts.metrics.cpu_percent') },
  { code: 'mem_percent', label: t('settings.alerts.metrics.mem_percent') },
  { code: 'disk_percent', label: t('settings.alerts.metrics.disk_percent') },
  { code: 'cpu_temp', label: t('settings.alerts.metrics.cpu_temp') },
  { code: 'swap_percent', label: t('settings.alerts.metrics.swap_percent') },
])

const operatorOptions = computed(() => [
  { code: 'gt', label: t('settings.alerts.operators.gt') },
  { code: 'lt', label: t('settings.alerts.operators.lt') },
])

const durationPresets = computed(() => [
  { code: 0, label: t('settings.alerts.durations.immediate') },
  { code: 60, label: t('settings.alerts.durations.1m') },
  { code: 300, label: t('settings.alerts.durations.5m') },
  { code: 900, label: t('settings.alerts.durations.15m') },
])

const metricIcons: Record<string, string> = {
  cpu_percent: 'Cpu',
  mem_percent: 'CircuitBoard',
  disk_percent: 'HardDrive',
  cpu_temp: 'Thermometer',
  swap_percent: 'CircuitBoard',
}

function metricUnit(metric: string): string {
  return metric === 'cpu_temp' ? '\u00b0C' : '%'
}

function operatorSymbol(op: string): string {
  switch (op) {
    case 'gt': return '>'
    case 'lt': return '<'
    case 'eq': return '='
    default: return op
  }
}

function formatDuration(seconds: number): string {
  if (seconds === 0) return t('settings.alerts.durations.immediate')
  if (seconds === 60) return t('settings.alerts.durations.1m')
  if (seconds === 300) return t('settings.alerts.durations.5m')
  if (seconds === 900) return t('settings.alerts.durations.15m')
  if (seconds < 60) return `${seconds}s`
  return `${Math.floor(seconds / 60)}m`
}

function formatDate(iso: string): string {
  return dayjs(iso).format('MMM D, YYYY HH:mm:ss')
}

function ruleLabelById(ruleId: string): string {
  const rule = rules.value.find((r) => r.id === ruleId)
  return rule?.label ?? ruleId
}

// ---- Data loading ----

async function loadData() {
  loading.value = true
  const [rulesResult, historyResult] = await Promise.all([
    safeCall(() => alertsApi.getRules()),
    safeCall(() => alertsApi.getHistory()),
  ])
  if (!rulesResult.error) rules.value = rulesResult.data ?? []
  if (!historyResult.error) history.value = historyResult.data ?? []
  loading.value = false
}

onMounted(loadData)

// ---- CRUD ----

function openAddDialog() {
  editingRule.value = null
  formMetric.value = 'cpu_percent'
  formOperator.value = 'gt'
  formThreshold.value = 90
  formDuration.value = 300
  formLabel.value = ''
  formEnabled.value = true
  formNotify.value = 'dashboard'
  dialogVisible.value = true
}

function openEditDialog(rule: AlertRule) {
  editingRule.value = rule
  formMetric.value = rule.metric
  formOperator.value = rule.operator
  formThreshold.value = rule.threshold
  formDuration.value = rule.duration_seconds
  formLabel.value = rule.label
  formEnabled.value = rule.enabled
  formNotify.value = rule.notify
  dialogVisible.value = true
}

async function saveRule() {
  const body = {
    metric: formMetric.value,
    operator: formOperator.value,
    threshold: formThreshold.value,
    duration_seconds: formDuration.value,
    enabled: formEnabled.value,
    notify: formNotify.value,
    label: formLabel.value,
  }

  if (editingRule.value) {
    const result = await safeCall(() =>
      alertsApi.updateRule(editingRule.value!.id, body),
    )
    if (!result.error) {
      const idx = rules.value.findIndex((r) => r.id === editingRule.value!.id)
      if (idx !== -1) rules.value[idx] = result.data
    }
  } else {
    const result = await safeCall(() => alertsApi.createRule(body))
    if (!result.error) {
      rules.value.push(result.data)
    }
  }
  dialogVisible.value = false
}

function confirmDelete(rule: AlertRule) {
  deletingRule.value = rule
  deleteDialogVisible.value = true
}

async function doDelete() {
  if (!deletingRule.value) return
  const result = await safeCall(() =>
    alertsApi.deleteRule(deletingRule.value!.id),
  )
  if (!result.error) {
    rules.value = rules.value.filter((r) => r.id !== deletingRule.value!.id)
  }
  deleteDialogVisible.value = false
  deletingRule.value = null
}

// Wave 34-Y: previously `toggleRule` was wired as
// `<ToggleSwitch v-model="rule.enabled" @update:model-value="toggleRule(rule)">`.
// That meant the local rule state was mutated BEFORE the backend
// was asked to persist the change — if the PUT failed (network
// error, backend validation, auth expired), the toggle remained in
// its new "on/off" position while the backend remained in the old
// state, and the user saw a silently-drifted view. The retry on
// navigation would then surprise them. New pattern: pass the
// requested next-state explicitly, call the backend, and only
// mutate the local rule.enabled on success. On failure we revert
// by not mutating (the ToggleSwitch is now unidirectional via
// :model-value).
async function toggleRule(rule: AlertRule, next: boolean) {
  const body = {
    metric: rule.metric,
    operator: rule.operator,
    threshold: rule.threshold,
    duration_seconds: rule.duration_seconds,
    enabled: next,
    notify: rule.notify,
    label: rule.label,
  }
  const result = await safeCall(() => alertsApi.updateRule(rule.id, body))
  if (!result.error) {
    rule.enabled = next
  }
}

async function testRule(rule: AlertRule) {
  const result = await safeCall(() => alertsApi.testRule(rule.id))
  if (!result.error) {
    history.value.unshift(result.data)
  }
}

async function clearHistory() {
  // History is stored server-side; for now we just clear the local view.
  // A dedicated endpoint could be added later.
  history.value = []
}
</script>

<template>
  <SettingsPage
    icon="BellRing"
    :title="t('settings.alerts.title')"
    :description="t('settings.alerts.description')"
  >
    <!-- Active Rules -->
    <SettingsCard
      :title="t('settings.alerts.rules_title')"
      :description="t('settings.alerts.rules_desc')"
    >
      <template #actions>
        <Button
          :label="t('settings.alerts.add_rule')"
          severity="primary"
          size="small"
          @click="openAddDialog"
        >
          <template #icon>
            <AppIcon name="Plus" :size="14" class="mr-1.5" />
          </template>
        </Button>
      </template>

      <div v-if="rules.length === 0" class="py-8 text-center text-sm text-[var(--cn-text-muted)]">
        <AppIcon name="Bell" :size="32" class="mx-auto mb-2 opacity-40" />
        {{ t('settings.alerts.no_rules') }}
      </div>

      <div v-else class="overflow-hidden rounded-lg border border-[var(--cn-border)]">
        <table class="w-full text-sm">
          <thead class="bg-[var(--cn-surface-hover)] text-xs uppercase tracking-wide text-[var(--cn-text-muted)]">
            <tr>
              <th class="w-12 px-3 py-2"></th>
              <th class="px-3 py-2 text-left font-medium">{{ t('settings.alerts.label') }}</th>
              <th class="hidden px-3 py-2 text-left font-medium sm:table-cell">{{ t('settings.alerts.metric') }}</th>
              <th class="hidden px-3 py-2 text-left font-medium md:table-cell">{{ t('settings.alerts.threshold') }}</th>
              <th class="hidden px-3 py-2 text-left font-medium lg:table-cell">{{ t('settings.alerts.duration') }}</th>
              <th class="px-3 py-2 text-left font-medium">{{ t('settings.alerts.enabled') }}</th>
              <th class="w-28 px-3 py-2"></th>
            </tr>
          </thead>
          <tbody class="divide-y divide-[var(--cn-border)]">
            <tr
              v-for="rule in rules"
              :key="rule.id"
              class="hover:bg-[var(--cn-surface-hover)]"
            >
              <td class="px-3 py-2.5">
                <AppIcon
                  :name="metricIcons[rule.metric] ?? 'AlertTriangle'"
                  :size="15"
                  class="text-blue-400"
                />
              </td>
              <td class="px-3 py-2.5 text-[var(--cn-text)]">{{ rule.label }}</td>
              <td class="hidden px-3 py-2.5 text-xs text-[var(--cn-text-muted)] sm:table-cell">
                {{ t(`settings.alerts.metrics.${rule.metric}`) }}
              </td>
              <td class="hidden px-3 py-2.5 font-mono text-xs text-[var(--cn-text-muted)] md:table-cell">
                {{ operatorSymbol(rule.operator) }} {{ rule.threshold }}{{ metricUnit(rule.metric) }}
              </td>
              <td class="hidden px-3 py-2.5 text-xs text-[var(--cn-text-muted)] lg:table-cell">
                <span class="inline-flex items-center gap-1">
                  <AppIcon name="Clock" :size="12" />
                  {{ formatDuration(rule.duration_seconds) }}
                </span>
              </td>
              <td class="px-3 py-2.5">
                <ToggleSwitch
                  :model-value="rule.enabled"
                  @update:model-value="(v) => toggleRule(rule, v as boolean)"
                />
              </td>
              <td class="px-3 py-2.5">
                <div class="flex items-center gap-1">
                  <button
                    class="rounded p-1.5 text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-blue-400"
                    :title="t('settings.alerts.test')"
                    @click="testRule(rule)"
                  >
                    <AppIcon name="Play" :size="13" />
                  </button>
                  <button
                    class="rounded p-1.5 text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-blue-400"
                    :title="t('settings.alerts.edit_rule')"
                    @click="openEditDialog(rule)"
                  >
                    <AppIcon name="Pencil" :size="13" />
                  </button>
                  <button
                    class="rounded p-1.5 text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-red-400"
                    :title="t('settings.alerts.delete_rule')"
                    @click="confirmDelete(rule)"
                  >
                    <AppIcon name="Trash2" :size="13" />
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </SettingsCard>

    <!-- Alert History -->
    <SettingsCard
      :title="t('settings.alerts.history_title')"
      :description="t('settings.alerts.history_desc')"
    >
      <template #actions>
        <Button
          v-if="history.length > 0"
          :label="t('settings.alerts.clear_history')"
          severity="secondary"
          size="small"
          @click="clearHistory"
        />
      </template>

      <div v-if="history.length === 0" class="py-8 text-center text-sm text-[var(--cn-text-muted)]">
        {{ t('settings.alerts.no_history') }}
      </div>

      <div v-else class="overflow-hidden rounded-lg border border-[var(--cn-border)]">
        <table class="w-full text-sm">
          <thead class="bg-[var(--cn-surface-hover)] text-xs uppercase tracking-wide text-[var(--cn-text-muted)]">
            <tr>
              <th class="w-10 px-3 py-2"></th>
              <th class="px-3 py-2 text-left font-medium">{{ t('settings.alerts.label') }}</th>
              <th class="hidden px-3 py-2 text-left font-medium sm:table-cell">{{ t('settings.alerts.threshold') }}</th>
              <th class="px-3 py-2 text-left font-medium">{{ t('settings.alerts.triggered') }}</th>
              <th class="hidden px-3 py-2 text-left font-medium md:table-cell">Status</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-[var(--cn-border)]">
            <tr
              v-for="(ev, idx) in history"
              :key="idx"
              class="hover:bg-[var(--cn-surface-hover)]"
            >
              <td class="px-3 py-2.5">
                <AppIcon
                  :name="ev.resolved_at ? 'CheckCircle2' : 'AlertTriangle'"
                  :size="15"
                  :class="ev.resolved_at ? 'text-emerald-400' : 'text-amber-400'"
                />
              </td>
              <td class="px-3 py-2.5 text-[var(--cn-text)]">{{ ruleLabelById(ev.rule_id) }}</td>
              <td class="hidden px-3 py-2.5 font-mono text-xs text-[var(--cn-text-muted)] sm:table-cell">
                {{ ev.value }}
              </td>
              <td class="px-3 py-2.5 font-mono text-xs text-[var(--cn-text-muted)]">
                {{ formatDate(ev.triggered_at) }}
              </td>
              <td class="hidden px-3 py-2.5 md:table-cell">
                <span
                  class="inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium"
                  :class="ev.resolved_at
                    ? 'bg-emerald-500/10 text-emerald-400'
                    : 'bg-amber-500/10 text-amber-400'"
                >
                  {{ ev.resolved_at ? t('settings.alerts.resolved') : t('settings.alerts.active') }}
                </span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </SettingsCard>

    <!-- Add/Edit Rule Dialog -->
    <Dialog
      v-model:visible="dialogVisible"
      modal
      :style="{ width: '28rem' }"
      :header="editingRule ? t('settings.alerts.edit_rule') : t('settings.alerts.add_rule')"
      :dismissable-mask="true"
    >
      <div class="flex flex-col gap-4">
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.alerts.label') }}
          </label>
          <InputText v-model="formLabel" class="w-full" placeholder="e.g. High CPU usage" />
        </div>

        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.alerts.metric') }}
          </label>
          <Select
            v-model="formMetric"
            :options="metricOptions"
            option-label="label"
            option-value="code"
            class="w-full"
          />
        </div>

        <div class="grid grid-cols-2 gap-3">
          <div>
            <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
              {{ t('settings.alerts.operator') }}
            </label>
            <Select
              v-model="formOperator"
              :options="operatorOptions"
              option-label="label"
              option-value="code"
              class="w-full"
            />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
              {{ t('settings.alerts.threshold') }}
              <span class="text-[var(--cn-text-muted)]">({{ metricUnit(formMetric) }})</span>
            </label>
            <InputNumber
              v-model="formThreshold"
              class="w-full"
              :min="0"
              :max="formMetric === 'cpu_temp' ? 150 : 100"
              :min-fraction-digits="0"
              :max-fraction-digits="1"
            />
          </div>
        </div>

        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.alerts.duration') }}
          </label>
          <Select
            v-model="formDuration"
            :options="durationPresets"
            option-label="label"
            option-value="code"
            class="w-full"
          />
        </div>

        <div class="flex items-center justify-between">
          <label class="text-sm text-[var(--cn-text)]">
            {{ t('settings.alerts.enabled') }}
          </label>
          <ToggleSwitch v-model="formEnabled" />
        </div>
      </div>

      <template #footer>
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          @click="dialogVisible = false"
        />
        <Button
          :label="t('common.save')"
          severity="primary"
          :disabled="!formLabel.trim()"
          @click="saveRule"
        />
      </template>
    </Dialog>

    <!-- Delete Confirmation Dialog -->
    <Dialog
      v-model:visible="deleteDialogVisible"
      modal
      :style="{ width: '24rem' }"
      :header="t('settings.alerts.delete_rule')"
      :dismissable-mask="true"
    >
      <p class="text-sm text-[var(--cn-text)]">
        {{ t('settings.alerts.delete_confirm') }}
      </p>
      <p v-if="deletingRule" class="mt-2 font-medium text-[var(--cn-text)]">
        {{ deletingRule.label }}
      </p>

      <template #footer>
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          @click="deleteDialogVisible = false"
        />
        <Button
          :label="t('common.delete')"
          severity="danger"
          @click="doDelete"
        />
      </template>
    </Dialog>
  </SettingsPage>
</template>
