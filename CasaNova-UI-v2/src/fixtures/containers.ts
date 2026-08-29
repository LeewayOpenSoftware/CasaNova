// App + service fixture.
//
// Shape mirrors CasaNova-AppManagement v0.9 compose responses. A
// CasaNova "app" is a compose project; each project contains 1..N
// services (containers). Single-container apps (Jellyfin, Pi-hole,
// Gitea) have exactly one service whose id matches the app id.
// Multi-container apps (Immich) have multiple services — the UI
// renders them as tabs in the app settings page.
//
// Two flat helpers (containersFixture + findContainer) are kept as a
// flattened projection so older call-sites that worked at the service
// level don't break during the refactor. New code should prefer
// appsFixture + findApp.
//
// This module owns FIXTURE DATA ONLY. The vocabulary (types) and the
// pure helpers live in @/types/containers and are re-exported here
// rather than redeclared. They used to be a byte-identical copy, which
// is exactly how a copy rots: aggregateAppState() was corrected over
// there so an unresolvable state falls back to 'stopped' instead of
// being painted green, and this file quietly kept answering 'running'.
// Nothing imported it yet, but a single definition is what guarantees
// a future import can't resurrect the old behaviour.

import type { AppEntry, ServiceEntry } from '@/types/containers'

export type {
  ContainerState,
  PortProtocol,
  PortMapping,
  VolumeMount,
  EnvVar,
  DeviceMapping,
  RestartPolicy,
  ServiceEntry,
  AppEntry,
} from '@/types/containers'

export { systemCapacityFixture, isRollingTag, aggregateAppState } from '@/types/containers'

// Canonical source of truth — every dashboard tile is one entry here.
// Single-container apps have services.length === 1; multi-container
// apps list every service in compose-declared order.
export const appsFixture: AppEntry[] = [
  {
    id: 'jellyfin',
    title: 'Jellyfin',
    description: 'The Free Software Media System. Stream your media to any device.',
    iconUrl: 'https://cdn.jsdelivr.net/gh/selfhst/icons/png/jellyfin.png',
    tileColor: '#7b4397',
    autoUpdate: true,
    webUIProtocol: 'http',
    webUIHost: '',
    webUIPort: 8096,
    webUIPath: '/',
    services: [
      {
        id: 'jellyfin',
        dockerImage: 'jellyfin/jellyfin',
        imageTag: '10.9.6',
        availableTags: ['latest', 'stable', '10.9.6', '10.9.5', '10.9.4', '10.8.13'],
        state: 'running',
        uptimeSeconds: 345_600,
        cpuPercent: 12,
        cpuLimitCores: 2,
        memoryMB: 842,
        memoryLimitMB: 2048,
        restartCount: 0,
        ports: [
          { host: 8096, container: 8096, protocol: 'tcp' },
          { host: 8920, container: 8920, protocol: 'tcp' },
          { host: 7359, container: 7359, protocol: 'udp' },
          { host: 1900, container: 1900, protocol: 'udp' },
        ],
        volumes: [
          { hostPath: '/DATA/AppData/jellyfin/config', containerPath: '/config', readOnly: false },
          { hostPath: '/DATA/AppData/jellyfin/cache', containerPath: '/cache', readOnly: false },
          { hostPath: '/DATA/Media', containerPath: '/media', readOnly: true },
        ],
        env: [
          { key: 'TZ', value: 'Europe/Madrid' },
          { key: 'PUID', value: '1000' },
          { key: 'PGID', value: '1000' },
          { key: 'JELLYFIN_PublishedServerUrl', value: 'https://jellyfin.home.local' },
        ],
        devices: [{ hostPath: '/dev/dri', containerPath: '/dev/dri' }],
        command: '',
        privileged: false,
        restartPolicy: 'unless-stopped',
        networks: ['bridge', 'jellyfin_default'],
      },
    ],
  },
  {
    id: 'immich',
    title: 'Immich',
    description: 'Self-hosted photo and video backup solution directly from your mobile phone.',
    iconUrl: 'https://cdn.jsdelivr.net/gh/selfhst/icons/png/immich.png',
    tileColor: '#4a5bf7',
    autoUpdate: true,
    webUIProtocol: 'http',
    webUIHost: '',
    webUIPort: 2283,
    webUIPath: '/',
    services: [
      {
        id: 'database',
        dockerImage: 'ghcr.io/immich-app/postgres',
        imageTag: '14-vectorchord0.3.0',
        availableTags: ['14-vectorchord0.3.0', '14-vectorchord0.2.0', '14'],
        state: 'running',
        uptimeSeconds: 172_800,
        cpuPercent: 2,
        cpuLimitCores: 1,
        memoryMB: 180,
        memoryLimitMB: 512,
        restartCount: 0,
        ports: [],
        volumes: [
          { hostPath: '/DATA/AppData/immich/postgres', containerPath: '/var/lib/postgresql/data', readOnly: false },
        ],
        env: [
          { key: 'POSTGRES_PASSWORD', value: '****************', secret: true },
          { key: 'POSTGRES_USER', value: 'postgres' },
          { key: 'POSTGRES_DB', value: 'immich' },
        ],
        devices: [],
        command: '',
        privileged: false,
        restartPolicy: 'unless-stopped',
        networks: ['immich_default'],
      },
      {
        id: 'immich-machine-learning',
        dockerImage: 'ghcr.io/immich-app/immich-machine-learning',
        imageTag: 'release',
        availableTags: ['release', 'latest', 'v1.106.4', 'v1.106.3', 'v1.105.1'],
        state: 'running',
        uptimeSeconds: 172_800,
        cpuPercent: 1,
        cpuLimitCores: 2,
        memoryMB: 620,
        memoryLimitMB: 2048,
        restartCount: 0,
        ports: [],
        volumes: [
          { hostPath: '/DATA/AppData/immich/ml-cache', containerPath: '/cache', readOnly: false },
        ],
        env: [{ key: 'TRANSFORMERS_CACHE', value: '/cache' }],
        devices: [],
        command: '',
        privileged: false,
        restartPolicy: 'unless-stopped',
        networks: ['immich_default'],
      },
      {
        id: 'immich-server',
        dockerImage: 'ghcr.io/immich-app/immich-server',
        imageTag: 'v1.106.4',
        availableTags: ['release', 'latest', 'v1.106.4', 'v1.106.3', 'v1.105.1'],
        state: 'running',
        uptimeSeconds: 172_800,
        cpuPercent: 3,
        cpuLimitCores: 2,
        memoryMB: 512,
        memoryLimitMB: 1024,
        restartCount: 0,
        ports: [{ host: 2283, container: 2283, protocol: 'tcp' }],
        volumes: [
          { hostPath: '/DATA/AppData/immich/upload', containerPath: '/usr/src/app/upload', readOnly: false },
          { hostPath: '/etc/localtime', containerPath: '/etc/localtime', readOnly: true },
        ],
        env: [
          { key: 'DB_HOSTNAME', value: 'database' },
          { key: 'DB_PASSWORD', value: '****************', secret: true },
          { key: 'DB_USERNAME', value: 'postgres' },
          { key: 'DB_DATABASE_NAME', value: 'immich' },
          { key: 'REDIS_HOSTNAME', value: 'redis' },
        ],
        devices: [],
        command: '',
        privileged: false,
        restartPolicy: 'unless-stopped',
        networks: ['immich_default'],
      },
      {
        id: 'redis',
        dockerImage: 'docker.io/redis',
        imageTag: '6.2-alpine',
        availableTags: ['latest', '7-alpine', '6.2-alpine', '6-alpine', '5-alpine'],
        state: 'running',
        uptimeSeconds: 172_800,
        cpuPercent: 0,
        cpuLimitCores: null,
        memoryMB: 18,
        memoryLimitMB: 128,
        restartCount: 0,
        ports: [],
        volumes: [],
        env: [],
        devices: [],
        command: '',
        privileged: false,
        restartPolicy: 'unless-stopped',
        networks: ['immich_default'],
      },
    ],
  },
  {
    id: 'nextcloud',
    title: 'Nextcloud',
    description: 'A safe home for all your data. Access & share your files, calendars, contacts, mail and more.',
    tileColor: '#0082c9',
    autoUpdate: false,
    webUIProtocol: 'http',
    webUIHost: '',
    webUIPort: 8080,
    webUIPath: '/',
    services: [
      {
        id: 'nextcloud',
        dockerImage: 'nextcloud',
        imageTag: '29.0.3',
        availableTags: ['latest', 'stable', '30', '29', '29.0.3', '29.0.2', '28'],
        state: 'unhealthy',
        uptimeSeconds: 86_400,
        cpuPercent: 3,
        cpuLimitCores: 1,
        memoryMB: 324,
        memoryLimitMB: 1024,
        restartCount: 2,
        ports: [{ host: 8080, container: 80, protocol: 'tcp' }],
        volumes: [
          { hostPath: '/DATA/AppData/nextcloud/data', containerPath: '/var/www/html/data', readOnly: false },
          { hostPath: '/DATA/AppData/nextcloud/config', containerPath: '/var/www/html/config', readOnly: false },
        ],
        env: [
          { key: 'NEXTCLOUD_ADMIN_USER', value: 'admin' },
          { key: 'NEXTCLOUD_ADMIN_PASSWORD', value: '****************', secret: true },
          { key: 'MYSQL_HOST', value: 'nextcloud-db' },
        ],
        devices: [],
        command: '',
        privileged: false,
        restartPolicy: 'unless-stopped',
        networks: ['nextcloud_default'],
      },
    ],
  },
  {
    id: 'pihole',
    title: 'Pi-hole',
    description: 'Network-wide ad blocking via your own DNS server.',
    tileColor: '#f60d1a',
    autoUpdate: true,
    webUIProtocol: 'http',
    webUIHost: '',
    webUIPort: 8081,
    webUIPath: '/admin',
    services: [
      {
        id: 'pihole',
        dockerImage: 'pihole/pihole',
        imageTag: '2024.07.0',
        availableTags: ['latest', '2024.07.0', '2024.06.0', '2024.05.0', 'development'],
        state: 'running',
        uptimeSeconds: 604_800,
        cpuPercent: 1,
        cpuLimitCores: null,
        memoryMB: 64,
        memoryLimitMB: 256,
        restartCount: 0,
        ports: [
          { host: 53, container: 53, protocol: 'both' },
          { host: 8081, container: 80, protocol: 'tcp' },
        ],
        volumes: [
          { hostPath: '/DATA/AppData/pihole/etc-pihole', containerPath: '/etc/pihole', readOnly: false },
          { hostPath: '/DATA/AppData/pihole/etc-dnsmasq', containerPath: '/etc/dnsmasq.d', readOnly: false },
        ],
        env: [
          { key: 'TZ', value: 'Europe/Madrid' },
          { key: 'WEBPASSWORD', value: '****************', secret: true },
        ],
        devices: [],
        command: '',
        privileged: false,
        restartPolicy: 'unless-stopped',
        networks: ['bridge'],
      },
    ],
  },
  {
    id: 'gitea',
    title: 'Gitea',
    description: 'Git with a cup of tea — a painless self-hosted Git service.',
    tileColor: '#609926',
    autoUpdate: false,
    webUIProtocol: 'http',
    webUIHost: '',
    webUIPort: 3000,
    webUIPath: '/',
    services: [
      {
        id: 'gitea',
        dockerImage: 'gitea/gitea',
        imageTag: '1.22.1',
        availableTags: ['latest', 'stable', '1.22.1', '1.22.0', '1.21.11', '1.21'],
        state: 'stopped',
        uptimeSeconds: 0,
        cpuPercent: 0,
        cpuLimitCores: 1,
        memoryMB: 0,
        memoryLimitMB: 512,
        restartCount: 0,
        ports: [
          { host: 3000, container: 3000, protocol: 'tcp' },
          { host: 2222, container: 22, protocol: 'tcp' },
        ],
        volumes: [{ hostPath: '/DATA/AppData/gitea', containerPath: '/data', readOnly: false }],
        env: [
          { key: 'USER_UID', value: '1000' },
          { key: 'USER_GID', value: '1000' },
        ],
        devices: [],
        command: '',
        privileged: false,
        restartPolicy: 'no',
        networks: ['gitea_default'],
      },
    ],
  },
]

/**
 * findApp — lookup helper for the app detail view.
 */
export function findApp(id: string): AppEntry | undefined {
  return appsFixture.find((a) => a.id === id)
}

// ------------------------------------------------------------------
// Back-compat flattened projection for code that worked at the
// service level during the single-container era. New code should read
// from appsFixture + findApp directly.
// ------------------------------------------------------------------

/**
 * ContainerEntry — legacy alias that flattens an app+service pair
 * into one row. Kept so the ContainersSettings page and older callers
 * continue to compile while the multi-service refactor lands.
 */
export interface ContainerEntry extends ServiceEntry {
  name: string // derived: app title + optional service suffix
  title: string // app title
  appId: string // owning app id
  description?: string
  image: string // combined "image:tag" for display
  autoUpdate: boolean
  webUIPort: number | null
  iconUrl?: string
  tileColor?: string
}

export const containersFixture: ContainerEntry[] = appsFixture.flatMap((app) =>
  app.services.map<ContainerEntry>((svc) => ({
    ...svc,
    name: app.services.length > 1 ? `${app.title} · ${svc.id}` : app.title,
    title: app.title,
    appId: app.id,
    description: app.description,
    image: `${svc.dockerImage}:${svc.imageTag}`,
    autoUpdate: app.autoUpdate,
    webUIPort: app.webUIPort,
    iconUrl: app.iconUrl,
    tileColor: app.tileColor,
  })),
)

export function findContainer(id: string): ContainerEntry | undefined {
  // Match by app id first (backwards compat with single-container apps
  // whose service id equals the app id), then fall back to service id.
  return (
    containersFixture.find((c) => c.appId === id && c.id === id) ??
    containersFixture.find((c) => c.id === id)
  )
}
