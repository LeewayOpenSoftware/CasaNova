<script setup lang="ts">
// Users & Roles settings — Stage 5.3 landing.
//
// Real interactive page: lists every user, shows per-row role badge,
// last-login timestamp, and an actions column with a role dropdown +
// delete button. An "Add user" button in the page header opens a
// modal with username, role, and optional email. Guard rails match
// the backend (v0.9 RBAC):
//   - can't delete the last admin
//   - can't demote the current user
//   - can't demote the last admin
// Current store is a fixture-backed Pinia store; swap to the real
// backend happens in a single api/users.ts layer later.

import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import Select from 'primevue/select'
import ToggleSwitch from 'primevue/toggleswitch'
import dayjs from 'dayjs'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import { useUsersStore } from '@/stores/users'
import type { UserEntry, UserRole } from '@/types/users'

const { t } = useI18n()
const users = useUsersStore()

// Wave 33: trigger the user-list fetch on page mount. The store used
// to fire `void refresh()` at module-evaluation time, but that ran
// before the auth guard had hydrated, spamming the backend with
// possibly-401 requests. Mounting this view is the only place the
// list is actually consumed, so this is the right place to fetch.
onMounted(() => {
  void users.refresh()
  // Only an administrator is served this list; for anyone else both endpoints
  // answer 403 and the store leaves systemDiskLoaded false, which is what
  // hides the column. Asking and being refused is how the UI learns which of
  // the two it is -- the role in the token is not authority over what the
  // backend will actually allow.
  void users.refreshSystemDisk()
})

// ----- System disk access -----
//
// "Can this account browse the whole disk from /?" It is OFF for everybody
// until an administrator turns it on, administrators included, and the guest
// account can never have it -- the backend refuses that outright, so the
// switch is rendered disabled rather than offered and then rejected.
const systemDiskBusy = ref<string | null>(null)

function canHoldSystemDisk(u: UserEntry): boolean {
  return u.role !== 'guest'
}

async function onSystemDiskChange(u: UserEntry, next: boolean) {
  if (!canHoldSystemDisk(u)) return
  systemDiskBusy.value = u.id
  try {
    await users.setSystemDiskAccess({ id: u.id, role: u.role }, next)
  } finally {
    systemDiskBusy.value = null
  }
}

// ----- Role display helpers -----
// Maps each role to an icon + tailwind color classes so the badges
// are visually distinct at a glance. Keyed by the same role enum the
// store uses so adding a role in one place updates both surfaces.
const roleVisuals: Record<UserRole, { icon: string; classes: string }> = {
  admin: { icon: 'ShieldCheck', classes: 'bg-blue-500/15 text-blue-500 border-blue-500/30' },
  user: { icon: 'User', classes: 'bg-emerald-500/15 text-emerald-500 border-emerald-500/30' },
  guest: { icon: 'Eye', classes: 'bg-slate-500/15 text-slate-400 border-slate-500/30' },
}

const roleOptions = computed(() =>
  (Object.keys(users.roles) as UserRole[]).map((r) => ({
    code: r,
    label: t(`settings.users.roles.${r}`),
  })),
)

function formatDate(iso: string | null): string {
  if (!iso) return t('settings.users.never')
  return dayjs(iso).format('MMM D, YYYY HH:mm')
}

// ----- Role change -----
// Returns true on success. The store's guards catch self-demotion and
// last-admin protection; we surface a toast-like error in a ref so the
// UI can show "Can't demote the last admin" without a full toast stack.
const errorMessage = ref<string | null>(null)

async function onRoleChange(user: UserEntry, newRole: UserRole) {
  errorMessage.value = null
  const ok = await users.changeRole(user.id, newRole)
  if (!ok) {
    if (user.id === users.currentUserId) {
      errorMessage.value = t('settings.users.errors.self_demote')
    } else {
      errorMessage.value = t('settings.users.errors.last_admin')
    }
  }
}

// ----- Delete flow -----
const deleteDialogVisible = ref(false)
const deleteTarget = ref<UserEntry | null>(null)

function requestDelete(user: UserEntry) {
  deleteTarget.value = user
  deleteDialogVisible.value = true
}

async function confirmDelete() {
  if (!deleteTarget.value) return
  const ok = await users.deleteUser(deleteTarget.value.id)
  if (!ok) {
    errorMessage.value = t('settings.users.errors.last_admin')
  }
  deleteDialogVisible.value = false
  deleteTarget.value = null
}

// ----- Create user flow -----
// The role reference is documentation, not a control: it sat permanently
// expanded under a 2-3 row table, pushing everything else off-screen. Collapsed
// by default, one click away when an admin is deciding what to assign.
const rolesOpen = ref(false)

const createDialogVisible = ref(false)
const newUsername = ref('')
const newEmail = ref('')
const newRole = ref<UserRole>('user')
const createError = ref<string | null>(null)

function openCreateDialog() {
  newUsername.value = ''
  newEmail.value = ''
  newRole.value = 'user'
  createError.value = null
  createDialogVisible.value = true
}

async function submitCreate() {
  createError.value = null
  if (!newUsername.value.trim()) {
    createError.value = t('settings.users.errors.username_required')
    return
  }
  const created = await users.createUser({
    username: newUsername.value,
    role: newRole.value,
    email: newEmail.value,
  })
  if (!created) {
    createError.value = t('settings.users.errors.duplicate')
    return
  }
  createDialogVisible.value = false
}

// Initial letter for the avatar placeholder — first non-space char,
// uppercased. Matches the style the dashboard topbar uses.
function initial(username: string): string {
  return username.trim().charAt(0).toUpperCase() || '?'
}
</script>

<template>
  <SettingsPage
    icon="Users"
    :title="t('settings.users.title')"
    :description="t('settings.users.description')"
  >
    <!-- Header actions: live user count + Add user button. Rendered in
         SettingsPage's trailing actions slot so they sit on the same
         baseline as the title, matching the visual rhythm of pages
         without actions. -->
    <template #actions>
      <span class="text-xs text-[var(--cn-text-muted)]">
        {{ t('settings.users.count', { n: users.entries.length }, users.entries.length) }}
      </span>
      <Button
        :label="t('settings.users.add')"
        size="small"
        @click="openCreateDialog"
      >
        <template #icon>
          <AppIcon name="UserPlus" :size="14" />
        </template>
      </Button>
    </template>

    <!-- Transient error banner for guard-rail violations. Auto-clears
         next time the user takes any action on the page. -->
    <div
      v-if="errorMessage"
      class="rounded-lg border border-red-500/30 bg-red-500/10 px-3 py-2 text-sm text-red-500"
    >
      {{ errorMessage }}
    </div>

    <!-- User table card -->
    <SettingsCard :title="t('settings.users.table_title')">
      <div class="overflow-hidden rounded-lg border border-[var(--cn-border)]">
        <table class="w-full text-sm">
          <thead class="bg-[var(--cn-surface-hover)] text-xs uppercase tracking-wide text-[var(--cn-text-muted)]">
            <tr>
              <th class="px-4 py-2 text-left font-medium">
                {{ t('settings.users.col_user') }}
              </th>
              <th class="hidden px-4 py-2 text-left font-medium sm:table-cell">
                {{ t('settings.users.col_role') }}
              </th>
              <th
                v-if="users.systemDiskLoaded"
                class="hidden px-4 py-2 text-left font-medium lg:table-cell"
              >
                {{ t('settings.users.col_system_disk') }}
              </th>
              <th class="hidden px-4 py-2 text-left font-medium md:table-cell">
                {{ t('settings.users.col_last_login') }}
              </th>
              <th class="w-1 px-4 py-2"></th>
            </tr>
          </thead>
          <tbody class="divide-y divide-[var(--cn-border)]">
            <tr
              v-for="u in users.sortedEntries"
              :key="u.id"
              class="hover:bg-[var(--cn-surface-hover)]"
            >
              <td class="px-4 py-3">
                <div class="flex items-center gap-3">
                  <div
                    class="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-blue-500/15 text-sm font-semibold text-blue-500"
                  >
                    {{ initial(u.username) }}
                  </div>
                  <div class="min-w-0">
                    <div class="flex items-center gap-2">
                      <span class="truncate font-medium text-[var(--cn-text)]">{{ u.username }}</span>
                      <span
                        v-if="u.id === users.currentUserId"
                        class="rounded-full bg-[var(--cn-surface-hover)] px-2 py-0.5 text-[10px] text-[var(--cn-text-muted)]"
                      >
                        {{ t('settings.users.you') }}
                      </span>
                    </div>
                    <div v-if="u.email" class="truncate text-xs text-[var(--cn-text-muted)]">{{ u.email }}</div>
                  </div>
                </div>
              </td>
              <td class="hidden px-4 py-3 sm:table-cell">
                <Select
                  :model-value="u.role"
                  :options="roleOptions"
                  option-label="label"
                  option-value="code"
                  class="w-40"
                  @update:model-value="(v: UserRole) => onRoleChange(u, v)"
                >
                  <template #value="{ value }">
                    <div v-if="value" class="inline-flex items-center gap-1.5">
                      <AppIcon :name="roleVisuals[value as UserRole].icon" :size="13" />
                      <span>{{ t(`settings.users.roles.${value}`) }}</span>
                    </div>
                  </template>
                </Select>
              </td>
              <td v-if="users.systemDiskLoaded" class="hidden px-4 py-3 lg:table-cell">
                <div class="flex items-center gap-2">
                  <ToggleSwitch
                    :model-value="users.hasSystemDiskAccess(u.id)"
                    :disabled="!canHoldSystemDisk(u) || systemDiskBusy === u.id"
                    :aria-label="t('settings.users.system_disk_aria', { user: u.username })"
                    @update:model-value="(v: boolean) => onSystemDiskChange(u, v)"
                  />
                  <span
                    v-if="!canHoldSystemDisk(u)"
                    class="text-xs text-[var(--cn-text-muted)]"
                  >
                    {{ t('settings.users.system_disk_never_guest') }}
                  </span>
                </div>
              </td>
              <td class="hidden px-4 py-3 text-[var(--cn-text-muted)] md:table-cell">
                {{ formatDate(u.lastLoginAt) }}
              </td>
              <td class="px-4 py-3 text-right">
                <button
                  type="button"
                  class="rounded-md p-1.5 text-[var(--cn-text-muted)] transition-colors hover:bg-red-500/10 hover:text-red-500"
                  :aria-label="t('settings.users.delete')"
                  :disabled="u.id === users.currentUserId"
                  :class="{ 'opacity-30 cursor-not-allowed hover:bg-transparent hover:text-[var(--cn-text-muted)]': u.id === users.currentUserId }"
                  @click="requestDelete(u)"
                >
                  <AppIcon name="Trash2" :size="15" />
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </SettingsCard>

    <!-- Role reference card — explains what each role can do so admins
         know what they're assigning before they assign it. Pulls straight
         from the rolesFixture description strings. -->
    <SettingsCard>
      <button
        type="button"
        class="flex w-full items-center gap-2 text-left text-sm font-medium text-[var(--cn-text)]"
        :aria-expanded="rolesOpen"
        @click="rolesOpen = !rolesOpen"
      >
        <AppIcon :name="rolesOpen ? 'ChevronDown' : 'ChevronRight'" :size="14" />
        {{ t('settings.users.roles_reference') }}
      </button>
      <ul v-if="rolesOpen" class="mt-3 space-y-3">
        <li
          v-for="meta in users.roles"
          :key="meta.id"
          class="rounded-lg border border-[var(--cn-border)] p-3"
        >
          <div class="flex items-center gap-2">
            <span
              class="inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs font-medium"
              :class="roleVisuals[meta.id as UserRole].classes"
            >
              <AppIcon :name="roleVisuals[meta.id as UserRole].icon" :size="12" />
              {{ t(`settings.users.roles.${meta.id}`) }}
            </span>
            <span class="text-xs text-[var(--cn-text-muted)]">
              {{ t('settings.users.permissions_count', { n: meta.permissionCount }) }}
            </span>
          </div>
          <p class="mt-2 text-xs text-[var(--cn-text-muted)]">{{ meta.label }}</p>
        </li>
      </ul>
    </SettingsCard>

    <!-- Create user dialog -->
    <Dialog
      v-model:visible="createDialogVisible"
      modal
      :style="{ width: '26rem' }"
      :header="t('settings.users.add_title')"
      :dismissable-mask="true"
    >
      <div class="flex flex-col gap-4">
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.users.username') }}
          </label>
          <InputText v-model="newUsername" class="w-full" autofocus />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.users.email_optional') }}
          </label>
          <InputText v-model="newEmail" class="w-full" />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.users.col_role') }}
          </label>
          <Select
            v-model="newRole"
            :options="roleOptions"
            option-label="label"
            option-value="code"
            class="w-full"
          />
        </div>
        <p v-if="createError" class="text-xs text-red-500">{{ createError }}</p>
      </div>
      <template #footer>
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          @click="createDialogVisible = false"
        />
        <Button :label="t('common.create')" @click="submitCreate" />
      </template>
    </Dialog>

    <!-- Delete confirm dialog -->
    <Dialog
      v-model:visible="deleteDialogVisible"
      modal
      :style="{ width: '22rem' }"
      :header="t('settings.users.delete_title')"
      :dismissable-mask="true"
    >
      <p class="text-sm text-[var(--cn-text)]">
        {{ t('settings.users.delete_confirm', { name: deleteTarget?.username ?? '' }) }}
      </p>
      <template #footer>
        <Button
          :label="t('common.cancel')"
          severity="secondary"
          @click="deleteDialogVisible = false"
        />
        <Button :label="t('common.delete')" severity="danger" @click="confirmDelete" />
      </template>
    </Dialog>
  </SettingsPage>
</template>
