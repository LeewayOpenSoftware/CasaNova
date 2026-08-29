// Mock filesystem for Stage 5 file manager development.
//
// Shape matches the CasaNova backend's ObjResp type from
// CasaNova/route/v1/file.go — so the swap to real API calls later is
// purely in the data-source layer (fixtures → api/files.ts → backend).
// Components consuming these types stay unchanged across the migration.
//
// The fixture models a small /DATA tree with a few folders, some
// images, documents, videos, and a hidden dotfile so the hidden-files
// toggle has something to reveal. Paths are absolute and keyed in a
// flat map so `listDir(path)` is O(1) during development.

export interface FileEntry {
  name: string
  path: string
  isDir: boolean
  size: number
  modified: string // ISO 8601
  // Free-form extensions blob matching the backend's ObjResp.extensions.
  // Used for share status, mount status, etc.
  extensions?: Record<string, unknown>
  /**
   * Static asset URL for fixture files that have real, viewable content
   * bundled under /public/assets/. Used by the preview modal to render
   * images + read text content for fixture entries the same way it
   * handles uploaded files (which go through URL.createObjectURL).
   * Undefined for fixture entries that are name-only placeholders.
   */
  url?: string
}

// Map of directory path → array of its children. Only directories are
// keys here; files are leaves with no entry. Empty folders have an
// empty array.
const TREE: Record<string, FileEntry[]> = {
  '/DATA': [
    {
      name: 'Documents',
      path: '/DATA/Documents',
      isDir: true,
      size: 0,
      modified: '2026-03-28T10:14:00Z',
    },
    {
      name: 'Downloads',
      path: '/DATA/Downloads',
      isDir: true,
      size: 0,
      modified: '2026-04-02T17:45:00Z',
    },
    {
      name: 'Gallery',
      path: '/DATA/Gallery',
      isDir: true,
      size: 0,
      modified: '2026-03-30T09:20:00Z',
    },
    {
      name: 'Media',
      path: '/DATA/Media',
      isDir: true,
      size: 0,
      modified: '2026-04-01T22:05:00Z',
    },
    {
      name: 'AppData',
      path: '/DATA/AppData',
      isDir: true,
      size: 0,
      modified: '2026-03-15T08:00:00Z',
    },
    {
      name: 'Backups',
      path: '/DATA/Backups',
      isDir: true,
      size: 0,
      modified: '2026-04-05T03:00:00Z',
    },
    // Hidden dotfile at the top of /DATA so the hidden-files toggle
    // has something visible to reveal without having to navigate deep.
    {
      name: '.config-notes.md',
      path: '/DATA/.config-notes.md',
      isDir: false,
      size: 1240,
      modified: '2026-02-10T11:30:00Z',
    },
    {
      name: 'welcome.txt',
      path: '/DATA/welcome.txt',
      isDir: false,
      size: 520,
      modified: '2026-01-01T00:00:00Z',
    },
  ],
  '/DATA/Documents': [
    {
      name: 'Taxes 2025',
      path: '/DATA/Documents/Taxes 2025',
      isDir: true,
      size: 0,
      modified: '2026-03-01T09:00:00Z',
    },
    {
      name: 'invoice-march.pdf',
      path: '/DATA/Documents/invoice-march.pdf',
      isDir: false,
      size: 483200,
      modified: '2026-03-28T10:14:00Z',
    },
    {
      name: 'roadmap.md',
      path: '/DATA/Documents/roadmap.md',
      isDir: false,
      size: 8300,
      modified: '2026-04-05T08:00:00Z',
    },
    {
      name: 'server-notes.txt',
      path: '/DATA/Documents/server-notes.txt',
      isDir: false,
      size: 2100,
      modified: '2026-03-22T14:00:00Z',
    },
  ],
  '/DATA/Documents/Taxes 2025': [
    {
      name: 'Q1.pdf',
      path: '/DATA/Documents/Taxes 2025/Q1.pdf',
      isDir: false,
      size: 2458112,
      modified: '2026-03-30T11:00:00Z',
    },
  ],
  '/DATA/Downloads': [
    {
      name: 'debian-12.5.0-amd64-netinst.iso',
      path: '/DATA/Downloads/debian-12.5.0-amd64-netinst.iso',
      isDir: false,
      size: 657457152,
      modified: '2026-04-02T17:45:00Z',
    },
    {
      name: 'firmware-update.zip',
      path: '/DATA/Downloads/firmware-update.zip',
      isDir: false,
      size: 12583040,
      modified: '2026-04-03T19:12:00Z',
    },
  ],
  // Gallery contains the 6 wallpaper assets from public/assets/wallpapers/.
  // Each entry has a real url so the preview modal renders the actual
  // image instead of the "no content" placeholder. File sizes are the
  // real sizes from the source files so the list view shows honest data.
  '/DATA/Gallery': [
    {
      name: 'mediterranean-living-room.png',
      path: '/DATA/Gallery/mediterranean-living-room.png',
      isDir: false,
      size: 2710474,
      modified: '2026-03-30T09:20:00Z',
      url: '/assets/wallpapers/wallpaper0a.png',
    },
    {
      name: 'sunlit-interior.png',
      path: '/DATA/Gallery/sunlit-interior.png',
      isDir: false,
      size: 2887509,
      modified: '2026-03-28T11:00:00Z',
      url: '/assets/wallpapers/wallpaper0b.png',
    },
    {
      name: 'courtyard-garden.png',
      path: '/DATA/Gallery/courtyard-garden.png',
      isDir: false,
      size: 3622267,
      modified: '2026-03-25T15:30:00Z',
      url: '/assets/wallpapers/wallpaper0c.png',
    },
    {
      name: 'terrace-afternoon.png',
      path: '/DATA/Gallery/terrace-afternoon.png',
      isDir: false,
      size: 3794585,
      modified: '2026-03-20T17:45:00Z',
      url: '/assets/wallpapers/wallpaper0d.png',
    },
    {
      name: 'garden-overview.png',
      path: '/DATA/Gallery/garden-overview.png',
      isDir: false,
      size: 3836777,
      modified: '2026-03-15T10:00:00Z',
      url: '/assets/wallpapers/wallpaper0e.png',
    },
    {
      name: 'village-path.png',
      path: '/DATA/Gallery/village-path.png',
      isDir: false,
      size: 2836942,
      modified: '2026-03-10T08:15:00Z',
      url: '/assets/wallpapers/wallpaper0z.png',
    },
  ],
  '/DATA/Media': [
    {
      name: 'movie-night.mp4',
      path: '/DATA/Media/movie-night.mp4',
      isDir: false,
      size: 1843000000,
      modified: '2026-04-01T22:05:00Z',
    },
    {
      name: 'playlist.m3u',
      path: '/DATA/Media/playlist.m3u',
      isDir: false,
      size: 1840,
      modified: '2026-03-20T12:00:00Z',
    },
  ],
  '/DATA/AppData': [
    {
      name: 'jellyfin',
      path: '/DATA/AppData/jellyfin',
      isDir: true,
      size: 0,
      modified: '2026-03-15T08:00:00Z',
    },
    {
      name: 'immich',
      path: '/DATA/AppData/immich',
      isDir: true,
      size: 0,
      modified: '2026-03-15T08:00:00Z',
    },
  ],
  '/DATA/AppData/jellyfin': [],
  '/DATA/AppData/immich': [],
  '/DATA/Backups': [
    {
      name: 'nextcloud-2026-04-05.restic',
      path: '/DATA/Backups/nextcloud-2026-04-05.restic',
      isDir: false,
      size: 48234516992,
      modified: '2026-04-05T03:00:00Z',
    },
  ],
}

/**
 * listDir — returns the entries under the given path, optionally
 * filtering out dotfiles. Mirrors the CasaNova backend's DirPath
 * handler semantics so components coded against this fixture work
 * unchanged once the real API wiring lands.
 *
 * @param path Absolute path to list. Must be a known directory.
 * @param showHidden If false (default), entries whose name starts with
 *   "." are filtered out — matching the backend's ?show_hidden=true
 *   query parameter.
 * @returns Array of entries, or an empty array for unknown paths
 *   (mirrors "directory does not exist" as an empty listing rather
 *   than an error for the dev-time flow).
 */
export function listDir(path: string, showHidden = false): FileEntry[] {
  const entries = TREE[path] ?? []
  if (showHidden) return entries
  return entries.filter((e) => !e.name.startsWith('.'))
}

/**
 * Favorites that show up in the sidebar's top section. These are
 * shortcuts into the mock filesystem and correspond roughly to the
 * "Favorites" section in the ZimaOS/CasaOS file manager references.
 */
export interface FavoriteEntry {
  labelKey: string
  path: string
  iconKey: 'documents' | 'downloads' | 'gallery' | 'media' | 'backup'
}

export const favoritesFixture: FavoriteEntry[] = [
  { labelKey: 'files.favorites.documents', path: '/DATA/Documents', iconKey: 'documents' },
  { labelKey: 'files.favorites.downloads', path: '/DATA/Downloads', iconKey: 'downloads' },
  { labelKey: 'files.favorites.gallery', path: '/DATA/Gallery', iconKey: 'gallery' },
  { labelKey: 'files.favorites.media', path: '/DATA/Media', iconKey: 'media' },
  { labelKey: 'files.favorites.backup', path: '/DATA/Backups', iconKey: 'backup' },
]

/**
 * Storage roots that show up in the sidebar's bottom section — the
 * mounted drives and cloud connections the user can navigate into.
 * The real app will populate this from GET /v2/local_storage/storage.
 */
export interface StorageRootEntry {
  id: string
  label: string
  path: string
  kind: 'internal' | 'external' | 'cloud'
}

export const storageRootsFixture: StorageRootEntry[] = [
  { id: 'data', label: 'DATA', path: '/DATA', kind: 'internal' },
]

/**
 * formatBytes — human-readable byte count for file sizes. Matches the
 * rendering convention used by CasaOS: TB for >= 1024GB, then GB, MB,
 * KB, bytes. Two decimal places on the leading three scales.
 */
export function formatBytes(bytes: number): string {
  if (bytes >= 1024 ** 4) return `${(bytes / 1024 ** 4).toFixed(2)} TB`
  if (bytes >= 1024 ** 3) return `${(bytes / 1024 ** 3).toFixed(2)} GB`
  if (bytes >= 1024 ** 2) return `${(bytes / 1024 ** 2).toFixed(2)} MB`
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${bytes} B`
}
