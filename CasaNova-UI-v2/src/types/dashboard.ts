// Dashboard types extracted from fixtures/dashboard.ts
// so production code can import these without pulling in fake data.

/**
 * AppGroup -- a folder that visually groups dashboard tiles. Purely a
 * UI concept stored in localStorage -- the backend knows nothing about
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
  // Background color for the icon tile when iconUrl is absent.
  tileColor: string
  status: 'running' | 'stopped' | 'updating' | 'unhealthy'
  /**
   * External URL opened in a new tab when the tile is clicked.
   */
  url?: string
  /**
   * Internal vue-router path. Takes precedence over `url` when present.
   */
  internalRoute?: string
  /**
   * When true, the tile renders a small "Preview" badge.
   */
  preview?: boolean
  /**
   * ID of the AppGroup this app belongs to. `undefined` means ungrouped.
   */
  groupId?: string
  /**
   * Which app source this tile came from (compose / standalone / external
   * stack). Lets the card route lifecycle to the right endpoints without
   * splitting the grid into separate user-facing worlds.
   */
  source?: import('@/types/containers').AppSource
}
