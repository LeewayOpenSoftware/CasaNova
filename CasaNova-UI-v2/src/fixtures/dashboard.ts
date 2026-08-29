// Local fixture data for Stage 3 dashboard development.
//
// These shapes mirror what the real CasaNova backend endpoints will
// eventually return, so swapping fixtures for API calls later is just
// changing the import. Every component that consumes a fixture also
// accepts a prop (so MSW / real backend wiring in a later stage is a
// one-line change per component, not a refactor).
//
// Fixtures live in their own directory (not colocated with components)
// so the production build can tree-shake them trivially when we cut
// them out post-integration.

export interface SystemStatsFixture {
  cpuPercent: number
  cpuTempC: number
  ramPercent: number
  ramUsedGB: number
  ramTotalGB: number
  uptimeSeconds: number
}

export const systemStatsFixture: SystemStatsFixture = {
  cpuPercent: 14,
  cpuTempC: 42,
  ramPercent: 38,
  ramUsedGB: 11.6,
  ramTotalGB: 30.65,
  uptimeSeconds: 60 * 60 * 24 * 7 + 60 * 60 * 3, // 7 days, 3 hours
}

export interface StorageFixture {
  label: string
  usedBytes: number
  totalBytes: number
  health: 'healthy' | 'warning' | 'critical'
}

// Matches the values from the CasaOS reference dashboard screenshot
// so the visual comparison is honest.
export const storageFixture: StorageFixture = {
  label: 'DATA',
  usedBytes: 1.27 * 1024 ** 4, // 1.27 TB
  totalBytes: 6.28 * 1024 ** 4, // 6.28 TB
  health: 'healthy',
}

/**
 * AppGroup — a folder that visually groups dashboard tiles. Purely a
 * UI concept stored in localStorage — the backend knows nothing about
 * groups. Each group has a display name, an optional accent color, and
 * a collapsed flag that controls whether the group renders as a single
 * folder tile or expands to show its children inline.
 */
export interface AppGroup {
  id: string
  name: string
  /** Accent color for the folder tile. Defaults to slate if omitted. */
  color?: string
  /** When true the group is rendered as a single compact folder tile;
   *  when false the group shows a header row + its children inline. */
  collapsed: boolean
}

export interface AppFixture {
  id: string
  name: string
  // Single-char fallback glyph until app icons are loaded from the store;
  // the real icon URL is the second field and is fetched lazily.
  initial: string
  iconUrl?: string
  // Background color for the icon tile when iconUrl is absent. Chosen to
  // give each fixture app a distinct visual identity in development.
  tileColor: string
  status: 'running' | 'stopped' | 'updating' | 'unhealthy'
  /**
   * External URL opened in a new tab when the tile is clicked. Used for
   * regular Docker apps that expose their own web UI.
   */
  url?: string
  /**
   * Internal vue-router path. Takes precedence over `url` when present,
   * and navigates within the SPA instead of opening a new tab. Used for
   * CasaNova-native surfaces like Virtual Machines, Terminal, Files —
   * anything that lives inside the app shell, not in an external container.
   */
  internalRoute?: string
  /**
   * When true, the tile renders a small "Preview" badge to communicate
   * that the feature is planned / incomplete. Used for stub tiles that
   * open NotImplementedView so users know not to expect a working feature.
   */
  preview?: boolean
  /**
   * ID of the AppGroup this app belongs to. `undefined` means the app
   * sits at the top level of the grid (ungrouped).
   */
  groupId?: string
}

// Six apps that collectively represent the common home-server workload:
// media, photos, dashboard, cloud, git, DNS. Arranged roughly in "most
// popular self-hosted app" order so the fixture looks realistic at a glance.
export const appsFixture: AppFixture[] = [
  {
    id: 'jellyfin',
    name: 'Jellyfin',
    initial: 'J',
    iconUrl: 'https://cdn.jsdelivr.net/gh/selfhst/icons/png/jellyfin.png',
    tileColor: '#7b4397',
    status: 'running',
    url: 'http://localhost:8096',
  },
  {
    id: 'immich',
    name: 'Immich',
    initial: 'I',
    iconUrl: 'https://cdn.jsdelivr.net/gh/selfhst/icons/png/immich.png',
    tileColor: '#4a5bf7',
    status: 'running',
  },
  {
    id: 'nextcloud',
    name: 'Nextcloud',
    initial: 'N',
    iconUrl: 'https://cdn.jsdelivr.net/gh/selfhst/icons/png/nextcloud.png',
    tileColor: '#0082c9',
    status: 'running',
  },
  {
    id: 'home-assistant',
    name: 'Home Assistant',
    initial: 'H',
    iconUrl: 'https://cdn.jsdelivr.net/gh/selfhst/icons/png/home-assistant.png',
    tileColor: '#18bcf2',
    status: 'updating',
  },
  {
    id: 'gitea',
    name: 'Gitea',
    initial: 'G',
    iconUrl: 'https://cdn.jsdelivr.net/gh/selfhst/icons/png/gitea.png',
    tileColor: '#609926',
    status: 'running',
  },
  {
    id: 'pihole',
    name: 'Pi-hole',
    initial: 'P',
    iconUrl: 'https://cdn.jsdelivr.net/gh/selfhst/icons/png/pi-hole.png',
    tileColor: '#f60d1a',
    status: 'unhealthy',
  },
  // CasaNova-native preview tile. Routes internally to the VM stub view
  // rather than opening an external URL. When VM support actually lands
  // (Phase 4 VM strategy) this entry transitions from `preview: true`
  // to a regular tile pointing at a real VM index page.
  {
    id: 'vms',
    name: 'Virtual Machines',
    initial: 'V',
    tileColor: '#475569', // slate-600
    status: 'stopped',
    internalRoute: '/app/vms',
  },
]
