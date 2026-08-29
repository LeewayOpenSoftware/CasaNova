<script setup lang="ts">
import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import client from '@/api/client'
import { safeCall } from '@/api/safeCall'

const props = defineProps<{
  visible: boolean
  backupCodesRemaining: number
}>()

const emit = defineEmits<{
  (e: 'update:visible', value: boolean): void
  (e: 'disabled'): void
}>()

const { t } = useI18n()

const code = ref('')
const loading = ref(false)
const error = ref('')

const dialogVisible = computed({
  get: () => props.visible,
  set: (val) => {
    if (!val) {
      code.value = ''
      error.value = ''
    }
    emit('update:visible', val)
  },
})

async function disable2FA() {
  error.value = ''
  loading.value = true

  const result = await safeCall(async () => {
    const res = await client.delete<{ success: number; message: string }>('/v1/sys/2fa', {
      data: { code: code.value },
    })
    return res.data
  })

  loading.value = false

  if (result.error) {
    error.value = result.error
  } else {
    dialogVisible.value = false
    emit('disabled')
  }
}
</script>

<template>
  <Dialog
    v-model:visible="dialogVisible"
    :header="t('settings.security.two_factor_disable_title')"
    modal
    :style="{ width: '420px' }"
    :pt="{ root: { class: 'cn-dialog' } }"
  >
    <div class="flex flex-col gap-5">
      <div class="text-sm text-[var(--cn-text-muted)]">
        {{ t('settings.security.two_factor_backup_remaining', { n: backupCodesRemaining }) }}
      </div>

      <div class="flex flex-col gap-2">
        <label class="text-sm text-[var(--cn-text)]">
          {{ t('settings.security.two_factor_disable_confirm') }}
        </label>
        <InputText
          v-model="code"
          :placeholder="'000000'"
          class="font-mono text-center text-lg tracking-[0.3em]"
          maxlength="8"
          @keydown.enter="disable2FA"
        />
        <div v-if="error" class="text-xs text-red-500">{{ error }}</div>
      </div>

      <div class="flex justify-end gap-2">
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          size="small"
          @click="dialogVisible = false"
        />
        <Button
          :label="t('settings.security.two_factor_disable')"
          severity="danger"
          size="small"
          :loading="loading"
          :disabled="code.length < 6"
          @click="disable2FA"
        >
          <template #icon>
            <AppIcon name="ShieldOff" :size="13" />
          </template>
        </Button>
      </div>
    </div>
  </Dialog>
</template>
