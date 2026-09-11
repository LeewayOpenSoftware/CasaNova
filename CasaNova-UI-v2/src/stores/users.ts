// Users store — fetches user data from the CasaNova-UserService backend.
//
// Endpoints used:
//   GET    /v1/users/name        → list all usernames
//   GET    /v1/users/:username   → full user object (id, role, created_at)
//   GET    /v1/users/roles       → available roles + permission bitmasks
//   DELETE /v1/users/:id         → delete a user
//   PUT    /v1/users/:id/role    → change role (body: {role})
//   POST   /v1/users/register    → bootstrap registration (requires one-shot key)
//
// Returns empty state when the backend is unreachable.

import { defineStore } from 'pinia'
import { ref, computed, onScopeDispose } from 'vue'
import { type UserEntry, type UserRole } from '@/types/users'
import client from '@/api/client'
import { safeCall } from '@/api/safeCall'
import { useAuthStore } from './auth'

interface RoleInfo {
  id: string
  label: string
  permissionCount: number
}

// Backend user shape returned by GET /v1/users/:username. Only the
// fields the UI consumes are typed; everything else (avatar, prefs,
// password hash, …) is ignored.
interface BackendUser {
  id: number
  username: string
  role: string
  created_at?: string
  email?: string
}

export const useUsersStore = defineStore('users', () => {
  const entries = ref<UserEntry[]>([])
  const roles = ref<RoleInfo[]>([])
  const fixtureMode = ref(false)
  const loading = ref(false)

  // Wave 33: previously this was `entries.find(u => u.role === 'admin')?.id`
  // — i.e. "the first admin in the list", which has nothing to do with
  // who is actually logged in. Two consequences:
  //
  //   1. The "(you)" badge in UsersSettings rendered on the wrong row.
  //   2. `:disabled="u.id === users.currentUserId"` on the delete button
  //      and the "demote yourself" guard in changeRole() were both
  //      gating off the wrong user. With two admins (Alice id=1 listed
  //      first, Bob id=2 currently logged in), Bob could click delete
  //      on his own row — the button was enabled, the API call ran,
  //      and Bob deleted himself. Conversely, when Bob tried to demote
  //      Alice, the guard fired with "cannot demote yourself".
  //
  // The actual "current user" is the one in the auth store, full stop.
  // Compare against the auth store's user.id (a number), normalised to
  // the same string format the entries use.
  const currentUserId = computed(() => {
    const auth = useAuthStore()
    return auth.user?.id != null ? String(auth.user.id) : null
  })

  const sortedEntries = computed(() =>
    [...entries.value].sort((a, b) => {
      const roleOrder = { admin: 0, user: 1, guest: 2 }
      const diff = roleOrder[a.role] - roleOrder[b.role]
      if (diff !== 0) return diff
      return a.username.localeCompare(b.username)
    }),
  )

  const adminCount = computed(() => entries.value.filter((u) => u.role === 'admin').length)

  async function refresh(): Promise<void> {
    loading.value = true

    // Fetch usernames first.
    const usersResult = await safeCall(
      async () => {
        const res = await client.get<{ success: number; data: string[] }>('/v1/users/name')
        return res.data.data ?? []
      },
    )

    // Fetch roles in parallel (independent of the username list).
    const rolesResult = await safeCall(
      async () => {
        const res = await client.get<{
          success: number
          data: Array<{ role: string; permissions: number }>
        }>('/v1/users/roles')
        return res.data.data ?? []
      },
    )

    if (usersResult.error) {
      loading.value = false
      fixtureMode.value = false
      return
    }

    // Hydrate each username with the full backend record so the store
    // carries real numeric IDs and roles. Without this hydration step,
    // delete and changeRole had nothing real to send to the API, so they
    // were silently no-op'd against fake `user-N` placeholders. Now the
    // store reflects backend state.
    const usernames = usersResult.data as string[]
    const hydrated: UserEntry[] = []
    for (const username of usernames) {
      const detail = await safeCall(async () => {
        const res = await client.get<{ success: number; data: BackendUser }>(
          '/v1/users/' + encodeURIComponent(username),
        )
        return res.data.data
      })
      if (detail.error || !detail.data) continue
      const u = detail.data
      hydrated.push({
        id: String(u.id),
        username: u.username,
        role: (u.role as UserRole) || 'user',
        email: u.email || undefined,
        createdAt: u.created_at || '',
        lastLoginAt: null,
      })
    }
    entries.value = hydrated

    loading.value = false
    fixtureMode.value = false

    // Update roles from backend
    if (!rolesResult.error && rolesResult.data) {
      const backendRoles = rolesResult.data as Array<{ role: string; permissions: number }>
      roles.value = backendRoles.map((r) => {
        // Count permission bits
        let bits = r.permissions
        let count = 0
        while (bits) { count += bits & 1; bits >>= 1 }
        return {
          id: r.role,
          label: r.role.charAt(0).toUpperCase() + r.role.slice(1),
          permissionCount: count,
        }
      })
    }
  }

  // Wave 33: do NOT trigger refresh() at module evaluation time. The
  // store is constructed by Pinia the first time anything imports it,
  // which on a cold dashboard load runs BEFORE the router guard has
  // hydrated auth state — and refresh() then walks every username
  // sequentially, spamming the backend with requests that may carry
  // a stale or missing Bearer token. UsersSettings.vue's onMounted
  // calls refresh() at the right time; that is the only caller that
  // actually needs the data.

  // Wave 33: clear the user list on logout so the next user logging
  // in on the same browser doesn't see the previous occupant's
  // username/email/role table during the brief window before the
  // Users page (if they ever open it) re-fetches.
  function _onAuthLogout() {
    entries.value = []
    roles.value = []
    loading.value = false
    fixtureMode.value = false
  }
  window.addEventListener('auth-logout', _onAuthLogout)
  onScopeDispose(() => {
    window.removeEventListener('auth-logout', _onAuthLogout)
  })

  // createUser is intentionally NOT wired to a backend call. The only
  // user-creation endpoint the backend exposes today is POST
  // /v1/users/register, which (a) requires a one-shot bootstrap key from
  // the host filesystem and (b) hard-codes the new account's role to
  // admin. That isn't a usable "admin provisions another user" flow, so
  // until a proper admin-create endpoint exists this returns null and
  // the UI surfaces the limitation rather than faking success against an
  // in-memory entry that disappears on refresh.
  async function createUser(_input: {
    username: string
    role: UserRole
    email?: string
  }): Promise<UserEntry | null> {
    return null
  }

  async function deleteUser(id: string): Promise<boolean> {
    const user = entries.value.find((u) => u.id === id)
    if (!user) return false
    // OLA 3 — this used to claim "the backend has the same check"; it did
    // not, until CasaNova-UserService's DeleteUser gained one (see
    // route/v1/user.go, countOtherAdmins). The backend is now the actual
    // guarantee — it also correctly handles legacy accounts whose stored
    // role isn't literally "admin" (see resolveEffectiveRole), which this
    // client-side check does not attempt to. This check stays only to fail
    // fast and keep the UI honest; it is not what makes deletion safe.
    if (user.role === 'admin' && adminCount.value <= 1) return false

    const result = await safeCall(async () => {
      await client.delete('/v1/users/' + encodeURIComponent(id))
      return true
    })
    if (result.error) return false

    entries.value = entries.value.filter((u) => u.id !== id)
    return true
  }

  async function changeRole(id: string, role: UserRole): Promise<boolean> {
    const user = entries.value.find((u) => u.id === id)
    if (!user) return false
    if (user.role === role) return true
    if (id === currentUserId.value && role !== 'admin') return false
    if (user.role === 'admin' && role !== 'admin' && adminCount.value <= 1) return false

    const result = await safeCall(async () => {
      await client.put('/v1/users/' + encodeURIComponent(id) + '/role', { role })
      return true
    })
    if (result.error) return false

    entries.value = entries.value.map((u) =>
      u.id === id ? { ...u, role } : u,
    )
    return true
  }

  return {
    entries,
    sortedEntries,
    roles,
    currentUserId,
    adminCount,
    fixtureMode,
    loading,
    refresh,
    createUser,
    deleteUser,
    changeRole,
  }
})
