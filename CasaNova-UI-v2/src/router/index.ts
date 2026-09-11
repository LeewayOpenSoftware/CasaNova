// Router configuration for CasaNova UI v2.
//
// Layout strategy: a single top-level /app route renders AppLayout (the
// TopBar + NotificationSidebar shell), and every authenticated screen is
// a child of that route. Unauthenticated screens (/login, /welcome) live
// at the root with no layout.
//
// URL layout stays short: / redirects into /app so the user-visible
// addresses are /app, /app/settings, /app/files, etc. The router guard
// consults the Pinia auth store and redirects unauthenticated users to
// /login with the intended destination in ?redirect= for post-login
// bounce-back.

import { createRouter, createWebHashHistory, type RouteRecordRaw } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { getUserStatus } from '@/api/users'
import { shouldDiscardExpiredSession } from './sessionExpiry'

// Cache the initialization check so we don't hit the API on every navigation.
// The promise acts as a dedup lock: concurrent navigations await the same call.
let _initChecked = false
let _initialized = true // assume true until proven otherwise
let _initPromise: Promise<void> | null = null

const routes: RouteRecordRaw[] = [
  {
    path: '/',
    redirect: '/app',
  },
  {
    path: '/welcome',
    name: 'welcome',
    component: () => import('@/views/WelcomeView.vue'),
    meta: { requiresAuth: false },
  },
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/LoginView.vue'),
    meta: { requiresAuth: false },
  },
  {
    path: '/app',
    component: () => import('@/components/layout/AppLayout.vue'),
    meta: { requiresAuth: true },
    children: [
      {
        path: '',
        name: 'home',
        component: () => import('@/views/HomeView.vue'),
      },
      // VM stub — routes to NotImplementedView via VmsView wrapper.
      // When VM support lands in Phase 4, VmsView becomes the real
      // feature's entry page and this route stays put.
      {
        path: 'vms',
        name: 'vms',
        component: () => import('@/views/VmsView.vue'),
        meta: { title: 'Virtual Machines' },
      },
      // Per-app detail + settings page. Reached from the dashboard
      // kebab "Settings" action and from the Containers settings
      // table row click. Parameterized by app id so every app has a
      // deep-linkable URL like /app/apps/jellyfin.
      {
        path: 'apps/:id',
        name: 'app-detail',
        component: () => import('@/views/apps/AppDetailView.vue'),
      },
      // Notes — a top-level section, not a corner of the file manager.
      //
      // The notes ARE files under /DATA/Notes, so nesting this under /app/files
      // would have been defensible. It is deliberately not: the point of the
      // feature is to reach a command you left on another machine in one click
      // from the top bar, and anything buried a level down defeats that.
      {
        path: 'notes',
        name: 'notes',
        component: () => import('@/views/notes/NotesView.vue'),
      },
      // Apps — an internal browser for the box's own apps. An island: it
      // reads the same installed-apps store the dashboard reads but renders
      // its own grid, and every app opens in a cross-origin iframe that can
      // reach neither the dashboard's localStorage nor /v1/*. The dashboard
      // itself is untouched by this section existing.
      {
        path: 'apps',
        name: 'apps',
        component: () => import('@/views/apps/AppsView.vue'),
      },
      // Files section — mirrors the SettingsLayout pattern: a parent
      // layout with sidebar + router outlet, plus one child view that
      // renders the browser itself. Keeping the sub-route structure
      // means future file-manager surfaces (trash, shares, search)
      // slot in as additional children without restructuring.
      {
        path: 'files',
        component: () => import('@/views/files/FilesLayout.vue'),
        children: [
          {
            path: '',
            name: 'files',
            component: () => import('@/views/files/FileBrowserView.vue'),
          },
          {
            path: 'shared',
            name: 'files-shared',
            component: () => import('@/views/files/SharedFoldersView.vue'),
          },
        ],
      },
      // Stage 4: Settings section with vertical-tab nav. The parent
      // route renders SettingsLayout (nav + router outlet). Default
      // child redirect lands on /general. Every domain is its own
      // lazy-imported page so the initial bundle stays small — adding
      // a new settings domain later is a new file + one route entry.
      {
        path: 'settings',
        component: () => import('@/views/settings/SettingsLayout.vue'),
        children: [
          { path: '', redirect: '/app/settings/general' },
          {
            path: 'general',
            component: () => import('@/views/settings/GeneralSettings.vue'),
          },
          {
            path: 'appearance',
            component: () => import('@/views/settings/AppearanceSettings.vue'),
          },
          {
            path: 'users',
            component: () => import('@/views/settings/UsersSettings.vue'),
          },
          {
            path: 'storage',
            component: () => import('@/views/settings/StorageSettings.vue'),
          },
          {
            path: 'apps',
            component: () => import('@/views/settings/ApplicationsSettings.vue'),
          },
          {
            path: 'containers',
            component: () => import('@/views/settings/ContainersSettings.vue'),
          },
          {
            path: 'app-stores',
            component: () => import('@/views/settings/AppStoresSettings.vue'),
          },
          {
            path: 'network',
            component: () => import('@/views/settings/NetworkSettings.vue'),
          },
          {
            path: 'security',
            component: () => import('@/views/settings/SecuritySettings.vue'),
          },
          {
            path: 'backups',
            component: () => import('@/views/settings/BackupsSettings.vue'),
          },
          {
            path: 'scheduled',
            component: () => import('@/views/settings/ScheduledSettings.vue'),
          },
          {
            path: 'alerts',
            component: () => import('@/views/settings/AlertsSettings.vue'),
          },
          {
            path: 'audit',
            component: () => import('@/views/settings/AuditSettings.vue'),
          },
          {
            path: 'logs',
            component: () => import('@/views/settings/LogsSettings.vue'),
          },
          {
            path: 'about',
            component: () => import('@/views/settings/AboutSettings.vue'),
          },
          {
            path: 'server',
            component: () => import('@/views/settings/ServerSettings.vue'),
          },
          {
            path: 'host',
            component: () => import('@/views/settings/HostSettings.vue'),
          },
          {
            path: 'updates',
            component: () => import('@/views/settings/UpdatesSettings.vue'),
          },
          {
            path: 'troubleshooter',
            component: () => import('@/views/settings/TroubleshooterSettings.vue'),
          },
          {
            path: 'maintenance',
            component: () => import('@/views/settings/MaintenanceSettings.vue'),
          },
          {
            path: 'diagnosis',
            component: () => import('@/views/settings/DiagnosisSettings.vue'),
          },
          {
            path: 'migration',
            component: () => import('@/views/settings/MigrationSettings.vue'),
          },
          {
            path: 'remote-access',
            component: () => import('@/views/settings/RemoteAccessSettings.vue'),
          },
                    {
            // Folded into Appearance. Kept as a redirect rather than deleted:
            // a bookmark or a link from an older release should land on the
            // section, not on a blank router miss.
            path: 'theme-packs',
            redirect: '/app/settings/appearance',
          },
        ],
      },
      // Stage 5 sub-pages slot in here as they're built. Each lazy-
      // imported so the initial bundle stays small.
    ],
  },
  {
    path: '/:pathMatch(.*)*',
    redirect: '/app',
  },
]

/**
 * Check whether a JWT access token is expired by decoding the payload
 * and comparing its `exp` claim against the current time. Returns true
 * for any token that can't be decoded (malformed, empty, etc.).
 *
 * Wave 32: previous implementation called `atob(token.split('.')[1])`
 * directly, but JWT (RFC 7519) uses base64**url** encoding which
 * substitutes `-` for `+` and `_` for `/` and omits padding. `atob`
 * only accepts standard base64 and throws DOMException on the
 * substituted characters. Statistically, almost every real token
 * payload contains at least one `-` or `_`, so this catch was firing
 * on every page navigation and the function returned `true`. The
 * router then called `auth.logout()` and force-kicked the user back
 * to the login screen — every reload, every route change. Decoded
 * symptom: "users complain they have to log in five times a day".
 */
const router = createRouter({
  history: createWebHashHistory(),
  routes,
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  auth.hydrate()

  // Keep a refreshable session alive: the first protected request will
  // receive 401 and the axios interceptor renews + retries it exactly once.
  // Without a refresh credential there is no recovery path, so clear it.
  if (auth.isAuthenticated && shouldDiscardExpiredSession(auth.accessToken, auth.refreshToken)) {
    auth.logout()
  }

  // On first navigation, check if the system has any users. If not,
  // redirect to /welcome so the admin can create the first account.
  // Uses a shared promise to dedup concurrent navigation guards.
  if (!_initChecked) {
    if (!_initPromise) {
      _initPromise = (async () => {
        try {
          const status = await getUserStatus()
          _initialized = status.data.initialized
          _initChecked = true
        } catch {
          // Backend unreachable — assume initialized for *this*
          // navigation so a transient API failure doesn't dump every
          // visitor onto /welcome (which would itself expose an admin-
          // creation form). Crucially, do NOT mark _initChecked=true:
          // before the fix, a single transient failure during the
          // first navigation cached the "initialized" state for the
          // entire SPA session, so an actually-uninitialized server
          // could be reached without ever triggering the welcome flow
          // again until reload. Leaving _initChecked false means the
          // next navigation will retry the probe, which is the
          // closest thing to "fail closed" we can do without making
          // the UI unusable when the backend is briefly down.
          _initialized = true
        } finally {
          _initPromise = null
        }
      })()
    }
    await _initPromise
  }

  if (!_initialized && to.name !== 'welcome') {
    return { name: 'welcome' }
  }

  // Walk the matched route chain — child routes don't inherit meta from
  // parents, so `to.meta.requiresAuth` would be undefined for children.
  const needsAuth = to.matched.some((r) => r.meta.requiresAuth)
  if (needsAuth && !auth.isAuthenticated) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  if (to.name === 'login' && auth.isAuthenticated) {
    return { path: '/app' }
  }
  return true
})

export default router
