<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import MorphIcon from '@/components/ui/MorphIcon.vue'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import client from '@/api/client'
import { safeCall } from '@/api/safeCall'

const props = defineProps<{
  visible: boolean
}>()

const emit = defineEmits<{
  (e: 'update:visible', value: boolean): void
  (e: 'enabled'): void
}>()

const { t } = useI18n()

// Steps: 'setup' | 'backup' | 'done'
const step = ref<'setup' | 'backup' | 'done'>('setup')
const secret = ref('')
const qrUri = ref('')
const code = ref('')
const loading = ref(false)
const error = ref('')
const backupCodes = ref<string[]>([])
const copied = ref(false)

const dialogVisible = computed({
  get: () => props.visible,
  set: (val) => emit('update:visible', val),
})

watch(
  () => props.visible,
  async (val) => {
    if (val) {
      step.value = 'setup'
      secret.value = ''
      qrUri.value = ''
      code.value = ''
      error.value = ''
      backupCodes.value = []
      copied.value = false
      loading.value = true

      const result = await safeCall(async () => {
        const res = await client.post<{
          success: number
          data: { secret: string; qr_uri: string }
        }>('/v1/sys/2fa/setup')
        return res.data.data
      })

      loading.value = false
      if (result.data) {
        secret.value = result.data.secret
        qrUri.value = result.data.qr_uri
      } else {
        error.value = result.error ?? 'Failed to start 2FA setup'
      }
    }
  },
)

async function verifyCode() {
  error.value = ''
  loading.value = true

  const result = await safeCall(async () => {
    const res = await client.post<{
      success: number
      message: string
      data: { success: boolean; backup_codes?: string[] }
    }>('/v1/sys/2fa/verify', { code: code.value })
    return res.data
  })

  loading.value = false

  if (result.data && result.data.data?.success) {
    backupCodes.value = result.data.data.backup_codes ?? []
    step.value = 'backup'
  } else {
    error.value = result.error ?? t('settings.security.two_factor_invalid_code')
  }
}

async function copyBackupCodes() {
  const text = backupCodes.value.join('\n')
  try {
    await navigator.clipboard.writeText(text)
  } catch {
    // Fallback for non-HTTPS contexts or denied permissions
    const ta = document.createElement('textarea')
    ta.value = text
    ta.style.position = 'fixed'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.focus()
    ta.select()
    try {
      document.execCommand('copy')
    } catch (err) {
      console.error('Clipboard copy failed:', err)
    }
    document.body.removeChild(ta)
  }
  copied.value = true
  setTimeout(() => (copied.value = false), 2000)
}

function finish() {
  dialogVisible.value = false
  emit('enabled')
}
</script>

<template>
  <Dialog
    v-model:visible="dialogVisible"
    :header="t('settings.security.two_factor_setup_title')"
    modal
    :closable="step !== 'backup'"
    :style="{ width: '480px' }"
    :pt="{ root: { class: 'cn-dialog' } }"
  >
    <!-- Step 1: Setup — show secret + code input -->
    <div v-if="step === 'setup'" class="flex flex-col gap-5">
      <div v-if="loading" class="flex items-center justify-center py-8">
        <div class="text-sm text-[var(--cn-text-muted)]">{{ t('common.loading') }}</div>
      </div>

      <template v-else-if="qrUri">
        <div class="text-sm text-[var(--cn-text-muted)]">
          {{ t('settings.security.two_factor_scan_qr') }}
        </div>

        <!-- QR URI as copyable monospace block -->
        <div class="rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-3">
          <div class="break-all font-mono text-xs text-[var(--cn-text)]">
            {{ qrUri }}
          </div>
        </div>

        <div class="text-sm text-[var(--cn-text-muted)]">
          {{ t('settings.security.two_factor_manual_entry') }}
        </div>

        <div class="flex items-center gap-2 rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-3 py-2">
          <span class="flex-1 select-all font-mono text-sm font-semibold tracking-wider text-[var(--cn-text)]">
            {{ secret }}
          </span>
        </div>

        <div class="flex flex-col gap-2">
          <label class="text-sm text-[var(--cn-text)]">
            {{ t('settings.security.two_factor_enter_code') }}
          </label>
          <InputText
            v-model="code"
            :placeholder="'000000'"
            class="font-mono text-center text-lg tracking-[0.3em]"
            maxlength="6"
            @keydown.enter="verifyCode"
          />
          <div v-if="error" class="text-xs text-red-500">{{ error }}</div>
        </div>
      </template>

      <div v-else-if="error" class="py-4 text-center text-sm text-red-500">
        {{ error }}
      </div>

      <div class="flex justify-end gap-2">
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          size="small"
          @click="dialogVisible = false"
        />
        <Button
          :label="t('settings.security.two_factor_verify')"
          size="small"
          :loading="loading"
          :disabled="code.length !== 6"
          @click="verifyCode"
        />
      </div>
    </div>

    <!-- Step 2: Backup codes -->
    <div v-else-if="step === 'backup'" class="flex flex-col gap-5">
      <div class="flex items-start gap-3 rounded-lg border border-amber-500/30 bg-amber-500/10 p-3">
        <AppIcon name="AlertTriangle" :size="18" class="mt-0.5 shrink-0 text-amber-500" />
        <div class="text-sm text-[var(--cn-text)]">
          {{ t('settings.security.two_factor_backup_warning') }}
        </div>
      </div>

      <div class="grid grid-cols-2 gap-2">
        <div
          v-for="bc in backupCodes"
          :key="bc"
          class="rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-3 py-2 text-center font-mono text-sm font-semibold tracking-wider text-[var(--cn-text)]"
        >
          {{ bc }}
        </div>
      </div>

      <div class="flex justify-end gap-2">
        <Button
          :label="copied ? t('settings.security.two_factor_copied') : t('settings.security.two_factor_copy_codes')"
          severity="secondary"
          size="small"
          @click="copyBackupCodes"
        >
          <template #icon>
            <MorphIcon :name="copied ? 'Check' : 'Copy'" :size="13" />
          </template>
        </Button>
        <Button
          :label="t('common.confirm')"
          size="small"
          @click="finish"
        />
      </div>
    </div>
  </Dialog>
</template>
