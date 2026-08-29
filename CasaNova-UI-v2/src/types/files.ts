// File types and utility functions extracted from fixtures/files.ts
// so production code can import these without pulling in fake data.

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

/**
 * formatBytes -- human-readable byte count for file sizes. Matches the
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
