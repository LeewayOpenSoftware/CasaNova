<script setup lang="ts">
// Login screen — modernized CasaOS paradigm.
//
// Layout mirrors the iconic CasaOS login: full-viewport wallpaper backdrop
// with a dark wash, centered semi-solid card containing an avatar circle,
// username + password fields, and a blue primary button. The avatar is a
// placeholder for now (the existing CasaOS "astronaut" asset will be
// re-drawn fresh to avoid legal ambiguity with IceWhale's original art).
//
// Validates the full stack: Pinia store + axios interceptor + router
// guard + i18n + PrimeVue components. Errors surface via Toast. On
// success the router redirects to ?redirect= target or /app.

import { ref, computed } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useToast } from 'primevue/usetoast'
import InputText from 'primevue/inputtext'
import Password from 'primevue/password'
import Button from 'primevue/button'
import { login } from '@/api/users'
import { useAuthStore } from '@/stores/auth'
import { AxiosError } from 'axios'

const { t } = useI18n()
const router = useRouter()
const route = useRoute()
const auth = useAuthStore()
const toast = useToast()

const username = ref('')
const password = ref('')
const submitting = ref(false)

const canSubmit = computed(
  () => username.value.trim() !== '' && password.value !== '' && !submitting.value,
)

async function onSubmit() {
  if (!canSubmit.value) return
  submitting.value = true
  try {
    const res = await login({
      username: username.value.trim(),
      password: password.value,
    })
    if (res.success !== 200) {
      toast.add({
        severity: 'error',
        summary: t('login.failed'),
        detail: res.message || t('login.check_credentials'),
        life: 4000,
      })
      return
    }
    auth.setSession(res.data.token.access_token, res.data.token.refresh_token, res.data.user)
    // Sanitize redirect to prevent open-redirect attacks (e.g. ?redirect=//evil.com).
    const raw = (route.query.redirect as string) || '/app'
    const redirect = raw.startsWith('/') && !raw.startsWith('//') ? raw : '/app'
    router.push(redirect)
  } catch (err) {
    const axiosErr = err as AxiosError<{ message?: string }>
    toast.add({
      severity: 'error',
      summary: t('login.failed'),
      detail: axiosErr.response?.data?.message || t('login.check_credentials'),
      life: 4000,
    })
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div>
    <!-- Full-viewport wallpaper backdrop. Default is a dusk gradient
         until wallpaper assets ship; users will be able to pick their
         own wallpaper later via Settings > General > Background. -->
    <div class="cn-login-backdrop" />

    <!-- Centered card over the wallpaper. Semi-solid so the wallpaper
         shows through slightly, but not translucent enough to hurt
         legibility of the form labels. -->
    <div class="relative flex min-h-screen items-center justify-center px-4">
      <div class="cn-login-card w-full max-w-md p-8">
        <!-- CasaNova brand mark in the avatar slot — the visual anchor
             of the login moment. The image is served from public/assets/brand/
             so it benefits from Vite's static serving without going through
             the bundler. Deferred: responsive srcset for mobile once the
             asset is resized to multiple widths (see
             public/assets/brand/README.md). -->
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

        <h1 class="mb-1 text-center text-2xl font-bold text-white">CasaNova</h1>
        <p class="mb-6 text-center text-sm text-slate-400">{{ t('login.subtitle') }}</p>

        <form class="space-y-4" @submit.prevent="onSubmit">
          <div>
            <label for="username" class="mb-1 block text-sm font-medium text-slate-300">
              {{ t('login.username') }}
            </label>
            <InputText
              id="username"
              v-model="username"
              :placeholder="t('login.username_placeholder')"
              class="w-full"
              autocomplete="username"
              autofocus
            />
          </div>
          <div>
            <label for="password" class="mb-1 block text-sm font-medium text-slate-300">
              {{ t('login.password') }}
            </label>
            <Password
              id="password"
              v-model="password"
              :placeholder="t('login.password_placeholder')"
              class="w-full"
              input-class="w-full"
              :feedback="false"
              toggle-mask
            />
          </div>
          <Button
            type="submit"
            :label="submitting ? t('login.signing_in') : t('login.sign_in')"
            :disabled="!canSubmit"
            class="w-full"
          />
        </form>
      </div>
    </div>
  </div>
</template>
