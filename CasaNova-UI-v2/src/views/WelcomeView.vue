<script setup lang="ts">
// Welcome / first-account-creation screen.
//
// Shown when the system is uninitialized (no users exist). Calls
// GET /v1/users/status to confirm the system is uninitialized and
// to obtain the path of the on-disk registration-key file. The
// admin must read the key out-of-band (SSH / console / migration
// script output) and paste it into this form.
//
// Wave 28: the key is no longer returned over HTTP — previously a
// remote attacker could fetch it from /v1/users/status before the
// real owner finished setup and register themselves as the first
// admin. The key now lives at the path returned in `key_file`
// (default: /var/lib/casanova/initial-registration-key.txt) and
// must be retrieved with host access.

import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useToast } from 'primevue/usetoast'
import InputText from 'primevue/inputtext'
import Password from 'primevue/password'
import Button from 'primevue/button'
import { getUserStatus, register, login } from '@/api/users'
import { useAuthStore } from '@/stores/auth'

const { t } = useI18n()
const router = useRouter()
const auth = useAuthStore()
const toast = useToast()

const username = ref('')
const password = ref('')
const confirmPassword = ref('')
const submitting = ref(false)
const registrationKey = ref('')
const keyFilePath = ref('')

onMounted(async () => {
  try {
    const status = await getUserStatus()
    if (status.data.initialized) {
      router.replace('/login')
      return
    }
    // The plaintext key is intentionally NOT in the response any
    // more (Wave 28). The backend returns key_file with the path
    // where the bootstrap key has been written; the admin must
    // retrieve it from the host and paste it below.
    keyFilePath.value = status.data.key_file || '/var/lib/casanova/initial-registration-key.txt'
    // Tolerate legacy backends that still return the key inline so
    // a partially-upgraded install isn't bricked at the welcome
    // screen.
    if (status.data.key) {
      registrationKey.value = status.data.key
    }
  } catch {
    // If we can't reach the backend, stay on the page
  }
})

const passwordError = computed(() => {
  if (password.value && password.value.length < 6) return t('welcome.password_too_short')
  if (confirmPassword.value && password.value !== confirmPassword.value) return t('welcome.password_mismatch')
  return ''
})

const canSubmit = computed(
  () =>
    username.value.trim() !== '' &&
    password.value.length >= 6 &&
    password.value === confirmPassword.value &&
    !submitting.value &&
    registrationKey.value !== '',
)

async function onSubmit() {
  if (!canSubmit.value) return
  submitting.value = true
  try {
    const res = await register({
      username: username.value.trim(),
      password: password.value,
      key: registrationKey.value,
    })
    if (res.success !== 200) {
      toast.add({
        severity: 'error',
        summary: t('welcome.failed'),
        detail: res.message,
        life: 4000,
      })
      return
    }

    toast.add({
      severity: 'success',
      summary: t('welcome.success'),
      life: 2000,
    })

    // Auto-login after registration
    const loginRes = await login({
      username: username.value.trim(),
      password: password.value,
    })
    if (loginRes.success === 200) {
      auth.setSession(
        loginRes.data.token.access_token,
        loginRes.data.token.refresh_token,
        loginRes.data.user,
      )
      router.replace('/app')
    } else {
      router.replace('/login')
    }
  } catch (err) {
    const e = err as { response?: { data?: { message?: string } } }
    toast.add({
      severity: 'error',
      summary: t('welcome.failed'),
      detail: e?.response?.data?.message || 'An unexpected error occurred.',
      life: 4000,
    })
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div>
    <div class="cn-login-backdrop" />

    <div class="relative flex min-h-screen items-center justify-center px-4">
      <div class="cn-login-card w-full max-w-md p-8">
        <h1 class="mb-1 text-center text-2xl font-bold text-white">
          {{ t('welcome.title') }}
        </h1>
        <p class="mb-6 text-center text-sm text-slate-400">
          {{ t('welcome.subtitle') }}
        </p>

        <div class="mb-6 flex justify-center">
          <div
            class="flex h-24 w-24 items-center justify-center rounded-full bg-slate-900/60 p-1 ring-2 ring-blue-500/40"
          >
            <img
              src="/assets/brand/casanova.png"
              alt="CasaNova"
              class="h-full w-full rounded-full object-cover"
            />
          </div>
        </div>

        <form class="space-y-4" @submit.prevent="onSubmit">
          <div v-if="keyFilePath" class="rounded border border-blue-500/40 bg-blue-500/10 p-3 text-xs text-slate-200">
            <p class="mb-1 font-semibold text-blue-500">First-time setup key required</p>
            <p>
              On the host, read the bootstrap key with:
              <code class="rounded bg-slate-900/60 px-1 py-0.5">sudo cat {{ keyFilePath }}</code>
            </p>
            <p class="mt-1 text-slate-400">Paste the contents into the field below.</p>
          </div>
          <div>
            <label for="reg-key" class="mb-1 block text-sm font-medium text-slate-300">
              Registration key
            </label>
            <InputText
              id="reg-key"
              v-model="registrationKey"
              placeholder="paste key from host file"
              class="w-full"
              autocomplete="off"
            />
          </div>
          <div>
            <label for="username" class="mb-1 block text-sm font-medium text-slate-300">
              {{ t('welcome.username') }}
            </label>
            <InputText
              id="username"
              v-model="username"
              :placeholder="t('welcome.username_placeholder')"
              class="w-full"
              autocomplete="username"
              autofocus
            />
          </div>
          <div>
            <label for="password" class="mb-1 block text-sm font-medium text-slate-300">
              {{ t('welcome.password') }}
            </label>
            <Password
              id="password"
              v-model="password"
              :placeholder="t('welcome.password_placeholder')"
              class="w-full"
              input-class="w-full"
              :feedback="false"
              toggle-mask
            />
          </div>
          <div>
            <label for="confirm-password" class="mb-1 block text-sm font-medium text-slate-300">
              {{ t('welcome.confirm_password') }}
            </label>
            <Password
              id="confirm-password"
              v-model="confirmPassword"
              :placeholder="t('welcome.confirm_placeholder')"
              class="w-full"
              input-class="w-full"
              :feedback="false"
              toggle-mask
            />
          </div>

          <p v-if="passwordError" class="text-sm text-red-400">{{ passwordError }}</p>

          <Button
            type="submit"
            :label="submitting ? t('welcome.creating') : t('welcome.create')"
            :disabled="!canSubmit"
            class="w-full"
          />
        </form>
      </div>
    </div>
  </div>
</template>
