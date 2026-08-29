<script setup lang="ts">
// UserAccountDialog — self-service account editor. Users open this
// from the topbar avatar menu to edit their username, change their
// password, or upload a new profile picture. Matches the CasaOS
// "Cuenta" sheet layout: big circular avatar at the top, two
// editable rows (Name / Password) with pencil inline-edit toggles,
// and a Sign out button in the top-right corner for quick exit.
//
// All mutations stay local to the auth store in the dev scaffold.
// Real backend wiring swaps in PUT /v1/users/me + password + avatar
// upload endpoints without touching the component contract.

import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useAuthStore } from '@/stores/auth'
import { useInstallProgressStore } from '@/stores/installProgress'
import { updateAvatar, fileToPngDataUrl, updatePassword } from '@/api/users'

const props = defineProps<{ visible: boolean }>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const { t } = useI18n()
const auth = useAuthStore()
const router = useRouter()
const installs = useInstallProgressStore()

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

// Edit state for each field — inline editing like the CasaOS layout.
const editingName = ref(false)
const editingPassword = ref(false)
const nameDraft = ref('')
const passwordDraft = ref('')
const oldPasswordDraft = ref('')
const savingPassword = ref(false)

// Open-edit helpers: seed the draft with the current value when the
// user enters edit mode so they can modify rather than retype.
function startEditName() {
  nameDraft.value = auth.user?.username ?? ''
  editingName.value = true
}
function confirmEditName() {
  const trimmed = nameDraft.value.trim()
  if (trimmed && auth.user) {
    // Use store action — real backend call: PUT /v1/users/me { username }
    auth.updateUser({ username: trimmed })
    installs.pushToast('success', t('account.name_updated'))
  }
  editingName.value = false
}
function cancelEditName() {
  editingName.value = false
}

function startEditPassword() {
  oldPasswordDraft.value = ''
  passwordDraft.value = ''
  editingPassword.value = true
}
async function confirmEditPassword() {
  if (passwordDraft.value.length < 6) {
    installs.pushToast('error', t('account.password_too_short'))
    return
  }
  if (!oldPasswordDraft.value) {
    installs.pushToast('error', t('account.old_password_required'))
    return
  }
  savingPassword.value = true
  try {
    // Real backend call — verifies the old password and revokes existing
    // sessions. Only toast success on an actual 2xx (the old code lied).
    await updatePassword(oldPasswordDraft.value, passwordDraft.value)
    installs.pushToast('success', t('account.password_updated'))
    oldPasswordDraft.value = ''
    passwordDraft.value = ''
    editingPassword.value = false
  } catch {
    installs.pushToast('error', t('account.password_change_failed'))
  } finally {
    savingPassword.value = false
  }
}
function cancelEditPassword() {
  editingPassword.value = false
  oldPasswordDraft.value = ''
  passwordDraft.value = ''
}

// Avatar upload: pick -> re-encode to a bounded PNG data URL -> stage as a
// preview -> Save PUTs it to /v1/users/avatar. On success we store the PNG
// data URL in the auth user so TopBar + this dialog update live and the
// picture survives a reload (the auth store persists user to localStorage).
// We deliberately ignore the filesystem path the backend echoes back — it
// is not a usable <img> src.
const fileInput = ref<HTMLInputElement | null>(null)
const avatarPreview = ref<string | null>(null)
const savingAvatar = ref(false)

// What the big circle shows: a staged preview wins, else the live avatar
// (data URL or authed GET url via the store), else null -> User icon.
const bigAvatar = computed(() =>
  avatarPreview.value ? avatarPreview.value : auth.user?.avatar ? auth.avatarSrc : null,
)

function pickAvatar() {
  fileInput.value?.click()
}
async function onAvatarChange(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return
  try {
    avatarPreview.value = await fileToPngDataUrl(file)
  } catch {
    installs.pushToast('error', t('account.avatar_invalid'))
  }
}
function cancelAvatar() {
  avatarPreview.value = null
}
async function saveAvatar() {
  if (!avatarPreview.value || !auth.user) return
  savingAvatar.value = true
  try {
    const res = await updateAvatar(avatarPreview.value)
    if (res.success !== 200) {
      installs.pushToast('error', res.message || t('account.avatar_failed'))
      return
    }
    auth.updateUser({ avatar: avatarPreview.value })
    avatarPreview.value = null
    installs.pushToast('success', t('account.avatar_updated'))
  } catch {
    installs.pushToast('error', t('account.avatar_failed'))
  } finally {
    savingAvatar.value = false
  }
}

function onLogout() {
  dialogVisible.value = false
  auth.logout()
  router.push({ name: 'login' })
}
</script>

<template>
  <Dialog
    v-model:visible="dialogVisible"
    modal
    :style="{ width: '28rem' }"
    :dismissable-mask="true"
  >
    <template #header>
      <div class="flex w-full items-center justify-between">
        <h2 class="text-lg font-semibold text-[var(--cn-text)]">{{ t('account.title') }}</h2>
        <Button
          :label="t('common.logout')"
          severity="danger"
          text
          size="small"
          @click="onLogout"
        >
          <template #icon><AppIcon name="LogOut" :size="14" /></template>
        </Button>
      </div>
    </template>

    <!-- Avatar with upload affordance + staged preview -->
    <div class="flex flex-col items-center gap-3 pb-4 pt-2">
      <div class="group relative">
        <div class="flex h-24 w-24 items-center justify-center overflow-hidden rounded-full bg-blue-500/15 text-blue-500">
          <img
            v-if="bigAvatar"
            :src="bigAvatar"
            :alt="auth.user?.username ?? ''"
            class="h-full w-full object-cover"
          />
          <AppIcon v-else name="User" :size="40" />
        </div>
        <button
          type="button"
          class="absolute bottom-0 right-0 flex h-8 w-8 items-center justify-center rounded-full border-2 border-[var(--cn-surface)] bg-blue-500 text-white shadow-md hover:bg-blue-600"
          :aria-label="t('account.upload_avatar')"
          :title="t('account.upload_avatar')"
          @click="pickAvatar"
        >
          <AppIcon name="Upload" :size="13" />
        </button>
        <input
          ref="fileInput"
          type="file"
          accept="image/*"
          class="hidden"
          @change="onAvatarChange"
        />
      </div>

      <!-- Save / Cancel appear only while a freshly picked image is staged. -->
      <div v-if="avatarPreview" class="flex items-center gap-2">
        <Button
          :label="t('account.avatar_save')"
          size="small"
          :loading="savingAvatar"
          @click="saveAvatar"
        />
        <Button
          :label="t('account.avatar_cancel')"
          size="small"
          text
          severity="secondary"
          :disabled="savingAvatar"
          @click="cancelAvatar"
        />
      </div>
    </div>

    <!-- Name row -->
    <div class="border-t border-[var(--cn-border)] py-4">
      <div class="mb-1 text-xs text-[var(--cn-text-muted)]">{{ t('account.name') }}</div>
      <div class="flex items-center gap-2">
        <template v-if="editingName">
          <InputText v-model="nameDraft" class="flex-1" autofocus @keyup.enter="confirmEditName" @keyup.escape="cancelEditName" />
          <button
            type="button"
            class="flex h-8 w-8 items-center justify-center rounded-md text-emerald-500 hover:bg-emerald-500/10"
            @click="confirmEditName"
          >
            <AppIcon name="Check" :size="14" />
          </button>
        </template>
        <template v-else>
          <div class="flex-1 text-base font-medium text-[var(--cn-text)]">
            {{ auth.user?.username ?? '—' }}
          </div>
          <button
            type="button"
            class="flex h-8 w-8 items-center justify-center rounded-md text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
            :aria-label="t('account.edit_name')"
            @click="startEditName"
          >
            <AppIcon name="Pencil" :size="14" />
          </button>
        </template>
      </div>
    </div>

    <!-- Password row -->
    <div class="border-t border-[var(--cn-border)] py-4">
      <div class="mb-1 text-xs text-[var(--cn-text-muted)]">{{ t('account.password') }}</div>
      <div class="flex items-center gap-2">
        <template v-if="editingPassword">
          <InputText
            v-model="oldPasswordDraft"
            type="password"
            :placeholder="t('account.old_password_placeholder')"
            class="flex-1"
            :disabled="savingPassword"
            @keyup.escape="cancelEditPassword"
          />
          <InputText
            v-model="passwordDraft"
            type="password"
            :placeholder="t('account.password_placeholder')"
            class="flex-1"
            autofocus
            :disabled="savingPassword"
            @keyup.enter="confirmEditPassword"
            @keyup.escape="cancelEditPassword"
          />
          <button
            type="button"
            class="flex h-8 w-8 items-center justify-center rounded-md text-emerald-500 hover:bg-emerald-500/10"
            @click="confirmEditPassword"
          >
            <AppIcon name="Check" :size="14" />
          </button>
        </template>
        <template v-else>
          <div class="flex-1 font-mono text-base text-[var(--cn-text-muted)]">••••••</div>
          <button
            type="button"
            class="flex h-8 w-8 items-center justify-center rounded-md text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
            :aria-label="t('account.edit_password')"
            @click="startEditPassword"
          >
            <AppIcon name="Pencil" :size="14" />
          </button>
        </template>
      </div>
    </div>
  </Dialog>
</template>
