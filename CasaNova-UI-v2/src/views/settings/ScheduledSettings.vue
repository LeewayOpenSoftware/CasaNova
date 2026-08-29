<script setup lang="ts">
// Scheduled Tasks settings — crontab management UI.
//
// Lists, creates, edits, deletes, and toggles system cron jobs via
// the /v1/sys/cron/* endpoints. Uses the same safeCall + fixture
// fallback pattern as the rest of the settings pages.

import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import InputText from 'primevue/inputtext'
import InputNumber from 'primevue/inputnumber'
import Select from 'primevue/select'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import ToggleSwitch from 'primevue/toggleswitch'
import { useToast } from 'primevue/usetoast'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { cronApi } from '@/api'
import { safeCall } from '@/api/safeCall'
import type { CronJob } from '@/api/cron'

const { t } = useI18n()
const toast = useToast()

// ---- State ----

const tasks = ref<CronJob[]>([])
const loading = ref(false)

// ---- Dialog state ----

const dialogVisible = ref(false)
const editingTask = ref<CronJob | null>(null)

const formDescription = ref('')
const formScheduleMode = ref<'preset' | 'custom'>('preset')
const formCronExpression = ref('0 * * * *')
const formPreset = ref('every_hour')
const formHour = ref(3)
const formMinute = ref(0)
const formDayOfWeek = ref(1)
const formDayOfMonth = ref(1)
const formCommand = ref('')

// Delete confirm
const deleteDialogVisible = ref(false)
const deletingTask = ref<CronJob | null>(null)

// ---- Preset options ----

const presetOptions = computed(() => [
  { code: 'every_hour', label: t('settings.scheduled.preset_every_hour') },
  { code: 'daily', label: t('settings.scheduled.preset_daily') },
  { code: 'weekly', label: t('settings.scheduled.preset_weekly') },
  { code: 'monthly', label: t('settings.scheduled.preset_monthly') },
])

const dayOfWeekOptions = computed(() => [
  { code: 1, label: t('settings.scheduled.day_mon') },
  { code: 2, label: t('settings.scheduled.day_tue') },
  { code: 3, label: t('settings.scheduled.day_wed') },
  { code: 4, label: t('settings.scheduled.day_thu') },
  { code: 5, label: t('settings.scheduled.day_fri') },
  { code: 6, label: t('settings.scheduled.day_sat') },
  { code: 0, label: t('settings.scheduled.day_sun') },
])

const scheduleModeOptions = computed(() => [
  { code: 'preset', label: t('settings.scheduled.schedule_mode_preset') },
  { code: 'custom', label: t('settings.scheduled.schedule_mode_custom') },
])

// ---- Human-readable cron ----

function humanizeCron(cron: string): string {
  const fields = cron.trim().split(/\s+/)
  if (fields.length !== 5) return t('settings.scheduled.human_custom')

  const [min, hour, dom, mon, dow] = fields

  // Every minute: * * * * *
  if (min === '*' && hour === '*' && dom === '*' && mon === '*' && dow === '*') {
    return t('settings.scheduled.human_every_minute')
  }

  // Every hour: 0 * * * * (or N * * * *)
  if (hour === '*' && dom === '*' && mon === '*' && dow === '*') {
    return t('settings.scheduled.human_every_hour')
  }

  const timeStr = `${hour.padStart(2, '0')}:${min.padStart(2, '0')}`

  // Daily: N N * * *
  if (dom === '*' && mon === '*' && dow === '*') {
    return t('settings.scheduled.human_daily_at', { time: timeStr })
  }

  // Weekly: N N * * D
  if (dom === '*' && mon === '*' && dow !== '*') {
    const dayNum = parseInt(dow, 10)
    const dayKey = ['day_sun', 'day_mon', 'day_tue', 'day_wed', 'day_thu', 'day_fri', 'day_sat'][dayNum] || 'day_mon'
    const dayName = t(`settings.scheduled.${dayKey}`)
    return t('settings.scheduled.human_weekly_on', { day: dayName, time: timeStr })
  }

  // Monthly: N N D * *
  if (mon === '*' && dow === '*' && dom !== '*') {
    return t('settings.scheduled.human_monthly_on', { day: dom, time: timeStr })
  }

  return t('settings.scheduled.human_custom')
}

// ---- Build cron from preset ----

function buildCronFromPreset(): string {
  const m = formMinute.value
  const h = formHour.value
  switch (formPreset.value) {
    case 'every_hour':
      return `0 * * * *`
    case 'daily':
      return `${m} ${h} * * *`
    case 'weekly':
      return `${m} ${h} * * ${formDayOfWeek.value}`
    case 'monthly':
      return `${m} ${h} ${formDayOfMonth.value} * *`
    default:
      return '0 * * * *'
  }
}

function getEffectiveSchedule(): string {
  return formScheduleMode.value === 'custom'
    ? formCronExpression.value
    : buildCronFromPreset()
}

const schedulePreview = computed(() => humanizeCron(getEffectiveSchedule()))

// ---- Parse cron into preset form fields ----

function parseCronIntoForm(cron: string) {
  const fields = cron.trim().split(/\s+/)
  if (fields.length !== 5) {
    formScheduleMode.value = 'custom'
    formCronExpression.value = cron
    return
  }

  const [min, hour, dom, , dow] = fields

  if (hour === '*' && dom === '*' && dow === '*') {
    formScheduleMode.value = 'preset'
    formPreset.value = 'every_hour'
    formMinute.value = 0
    formHour.value = 0
  } else if (dom === '*' && dow === '*') {
    formScheduleMode.value = 'preset'
    formPreset.value = 'daily'
    formMinute.value = parseInt(min, 10) || 0
    formHour.value = parseInt(hour, 10) || 0
  } else if (dom === '*' && dow !== '*') {
    formScheduleMode.value = 'preset'
    formPreset.value = 'weekly'
    formMinute.value = parseInt(min, 10) || 0
    formHour.value = parseInt(hour, 10) || 0
    formDayOfWeek.value = parseInt(dow, 10) || 0
  } else if (dow === '*' && dom !== '*') {
    formScheduleMode.value = 'preset'
    formPreset.value = 'monthly'
    formMinute.value = parseInt(min, 10) || 0
    formHour.value = parseInt(hour, 10) || 0
    formDayOfMonth.value = parseInt(dom, 10) || 1
  } else {
    formScheduleMode.value = 'custom'
    formCronExpression.value = cron
  }
}

// ---- API calls ----

async function loadTasks() {
  loading.value = true
  const result = await safeCall(
    () => cronApi.getJobs(),
    () => [],
  )
  if (result.error) {
    toast.add({ severity: 'error', summary: t('settings.scheduled.error_load'), life: 4000 })
  } else {
    tasks.value = result.data
  }
  loading.value = false
}

async function toggleTask(task: CronJob) {
  const result = await safeCall(() => cronApi.toggleJob(task.id))
  if (result.error) {
    toast.add({ severity: 'error', summary: t('settings.scheduled.error_toggle'), life: 4000 })
  } else {
    await loadTasks()
  }
}

function openAddDialog() {
  editingTask.value = null
  formDescription.value = ''
  formScheduleMode.value = 'preset'
  formPreset.value = 'every_hour'
  formHour.value = 3
  formMinute.value = 0
  formDayOfWeek.value = 1
  formDayOfMonth.value = 1
  formCronExpression.value = '0 3 * * *'
  formCommand.value = ''
  dialogVisible.value = true
}

function openEditDialog(task: CronJob) {
  editingTask.value = task
  formDescription.value = task.description
  formCommand.value = task.command
  parseCronIntoForm(task.schedule)
  dialogVisible.value = true
}

// Creating a cron job is not idempotent: two clicks on Save while the first
// POST is still in flight produce two identical jobs that then both run on
// schedule. Guard the handler and reflect it in the button.
const saving = ref(false)

async function saveTask() {
  if (saving.value) return
  saving.value = true
  try {
    await doSaveTask()
  } finally {
    saving.value = false
  }
}

async function doSaveTask() {
  const schedule = getEffectiveSchedule()
  const payload = {
    schedule,
    command: formCommand.value,
    description: formDescription.value,
  }

  if (editingTask.value) {
    const result = await safeCall(() => cronApi.updateJob(editingTask.value!.id, payload))
    if (result.error) {
      toast.add({ severity: 'error', summary: t('settings.scheduled.error_save'), life: 4000 })
      return
    }
    toast.add({ severity: 'success', summary: t('settings.scheduled.success_updated'), life: 3000 })
  } else {
    const result = await safeCall(() => cronApi.createJob(payload))
    if (result.error) {
      toast.add({ severity: 'error', summary: t('settings.scheduled.error_save'), life: 4000 })
      return
    }
    toast.add({ severity: 'success', summary: t('settings.scheduled.success_created'), life: 3000 })
  }

  dialogVisible.value = false
  await loadTasks()
}

function openDeleteDialog(task: CronJob) {
  deletingTask.value = task
  deleteDialogVisible.value = true
}

async function confirmDelete() {
  if (!deletingTask.value) return
  const result = await safeCall(() => cronApi.deleteJob(deletingTask.value!.id))
  if (result.error) {
    toast.add({ severity: 'error', summary: t('settings.scheduled.error_delete'), life: 4000 })
  } else {
    toast.add({ severity: 'success', summary: t('settings.scheduled.success_deleted'), life: 3000 })
  }
  deleteDialogVisible.value = false
  deletingTask.value = null
  await loadTasks()
}

onMounted(loadTasks)
</script>

<template>
  <SettingsPage
    icon="Calendar"
    :title="t('settings.scheduled.title')"
    :description="t('settings.scheduled.description')"
  >
    <div class="flex items-center justify-between">
      <div class="text-sm text-[var(--cn-text-muted)]">
        {{ t('settings.scheduled.count', { n: tasks.length }, tasks.length) }}
      </div>
      <Button :label="t('settings.scheduled.add')" size="small" @click="openAddDialog">
        <template #icon>
          <AppIcon name="Plus" :size="14" />
        </template>
      </Button>
    </div>

    <SettingsCard>
      <div class="overflow-hidden rounded-lg border border-[var(--cn-border)]">
        <table class="w-full text-sm">
          <thead class="bg-[var(--cn-surface-hover)] text-xs uppercase tracking-wide text-[var(--cn-text-muted)]">
            <tr>
              <th class="w-1 px-4 py-2 text-center font-medium">{{ t('settings.scheduled.col_enabled') }}</th>
              <th class="px-4 py-2 text-left font-medium">{{ t('settings.scheduled.col_task') }}</th>
              <th class="hidden px-4 py-2 text-left font-medium sm:table-cell">{{ t('settings.scheduled.col_schedule') }}</th>
              <th class="hidden px-4 py-2 text-left font-medium md:table-cell">{{ t('settings.scheduled.col_command') }}</th>
              <th class="w-1 px-4 py-2 text-right font-medium">{{ t('settings.scheduled.col_actions') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-[var(--cn-border)]">
            <tr v-for="task in tasks" :key="task.id" class="hover:bg-[var(--cn-surface-hover)]">
              <td class="px-4 py-3 text-center">
                <ToggleSwitch
                  :model-value="task.enabled"
                  @update:model-value="toggleTask(task)"
                />
              </td>
              <td class="px-4 py-3">
                <div class="text-[var(--cn-text)]">{{ task.description || task.command }}</div>
                <div v-if="task.description" class="truncate font-mono text-xs text-[var(--cn-text-muted)]">
                  {{ task.command }}
                </div>
              </td>
              <td class="hidden px-4 py-3 text-xs text-[var(--cn-text-muted)] sm:table-cell">
                <div>{{ humanizeCron(task.schedule) }}</div>
                <div class="font-mono text-[10px]">{{ task.schedule }}</div>
              </td>
              <td class="hidden px-4 py-3 md:table-cell">
                <code class="text-xs text-[var(--cn-text-muted)]">{{ task.command }}</code>
              </td>
              <td class="px-4 py-3 text-right">
                <div class="flex items-center justify-end gap-1">
                  <button
                    type="button"
                    class="rounded-md p-1.5 text-[var(--cn-text-muted)] transition-colors hover:bg-blue-500/10 hover:text-blue-500"
                    @click="openEditDialog(task)"
                  >
                    <AppIcon name="Pencil" :size="15" />
                  </button>
                  <button
                    type="button"
                    class="rounded-md p-1.5 text-[var(--cn-text-muted)] transition-colors hover:bg-red-500/10 hover:text-red-500"
                    @click="openDeleteDialog(task)"
                  >
                    <AppIcon name="Trash2" :size="15" />
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
        <div
          v-if="tasks.length === 0"
          class="py-8 text-center"
        >
          <AppIcon name="Clock" :size="32" class="mx-auto mb-2 text-[var(--cn-text-muted)]" />
          <div class="text-sm font-medium text-[var(--cn-text)]">
            {{ t('settings.scheduled.empty_title') }}
          </div>
          <div class="mt-1 text-xs text-[var(--cn-text-muted)]">
            {{ t('settings.scheduled.empty_desc') }}
          </div>
        </div>
      </div>
    </SettingsCard>

    <!-- Add / Edit dialog -->
    <Dialog
      v-model:visible="dialogVisible"
      :header="editingTask ? t('settings.scheduled.edit_title') : t('settings.scheduled.add_title')"
      modal
      :style="{ width: '32rem' }"
    >
      <div class="flex flex-col gap-4">
        <!-- Description -->
        <div class="flex flex-col gap-1">
          <label class="text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.scheduled.description_label') }}
          </label>
          <InputText
            v-model="formDescription"
            :placeholder="t('settings.scheduled.description_placeholder')"
            class="w-full"
          />
        </div>

        <!-- Schedule mode -->
        <div class="flex flex-col gap-1">
          <label class="text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.scheduled.schedule_label') }}
          </label>
          <Select
            v-model="formScheduleMode"
            :options="scheduleModeOptions"
            option-label="label"
            option-value="code"
            class="w-full"
          />
        </div>

        <!-- Preset controls -->
        <div v-if="formScheduleMode === 'preset'" class="flex flex-col gap-3">
          <Select
            v-model="formPreset"
            :options="presetOptions"
            option-label="label"
            option-value="code"
            class="w-full"
          />

          <!-- Time picker (for daily, weekly, monthly) -->
          <div v-if="formPreset !== 'every_hour'" class="flex items-center gap-2">
            <InputNumber
              v-model="formHour"
              :min="0"
              :max="23"
              class="w-20"
              show-buttons
              button-layout="horizontal"
              :step="1"
            />
            <span class="text-[var(--cn-text-muted)]">:</span>
            <InputNumber
              v-model="formMinute"
              :min="0"
              :max="59"
              class="w-20"
              show-buttons
              button-layout="horizontal"
              :step="5"
            />
          </div>

          <!-- Day of week (for weekly) -->
          <Select
            v-if="formPreset === 'weekly'"
            v-model="formDayOfWeek"
            :options="dayOfWeekOptions"
            option-label="label"
            option-value="code"
            class="w-full"
          />

          <!-- Day of month (for monthly) -->
          <InputNumber
            v-if="formPreset === 'monthly'"
            v-model="formDayOfMonth"
            :min="1"
            :max="28"
            class="w-24"
            show-buttons
          />
        </div>

        <!-- Custom cron expression -->
        <div v-if="formScheduleMode === 'custom'" class="flex flex-col gap-1">
          <label class="text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.scheduled.cron_expression') }}
          </label>
          <InputText
            v-model="formCronExpression"
            :placeholder="t('settings.scheduled.cron_placeholder')"
            class="w-full font-mono"
          />
        </div>

        <!-- Schedule preview -->
        <div class="rounded-lg bg-[var(--cn-surface-hover)] px-3 py-2">
          <div class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.scheduled.preview_label') }}</div>
          <div class="text-sm font-medium text-[var(--cn-text)]">
            <AppIcon name="Clock" :size="14" class="mr-1 inline-block" />
            {{ schedulePreview }}
          </div>
        </div>

        <!-- Command -->
        <div class="flex flex-col gap-1">
          <label class="text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.scheduled.command_label') }}
          </label>
          <InputText
            v-model="formCommand"
            :placeholder="t('settings.scheduled.command_placeholder')"
            class="w-full font-mono"
          />
        </div>
      </div>

      <template #footer>
        <div class="flex justify-end gap-2">
          <Button
            :label="t('settings.scheduled.cancel')"
            severity="secondary"
            text
            @click="dialogVisible = false"
          />
          <Button
            :label="t('settings.scheduled.save')"
            :disabled="!formCommand.trim() || saving"
            :loading="saving"
            @click="saveTask"
          />
        </div>
      </template>
    </Dialog>

    <!-- Delete confirmation -->
    <Dialog
      v-model:visible="deleteDialogVisible"
      :header="t('settings.scheduled.delete_title')"
      modal
      :style="{ width: '24rem' }"
    >
      <p class="text-sm text-[var(--cn-text)]">
        {{ t('settings.scheduled.delete_confirm', { name: deletingTask?.description || deletingTask?.command || '' }) }}
      </p>
      <template #footer>
        <div class="flex justify-end gap-2">
          <Button
            :label="t('settings.scheduled.cancel')"
            severity="secondary"
            text
            @click="deleteDialogVisible = false"
          />
          <Button
            :label="t('settings.scheduled.delete')"
            severity="danger"
            @click="confirmDelete"
          />
        </div>
      </template>
    </Dialog>
  </SettingsPage>
</template>
