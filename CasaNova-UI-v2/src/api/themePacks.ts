// Theme pack manager — upload, list, delete user-supplied theme packs.
// See backend at CasaNova/route/v1/theme_packs.go for the pack format.

import client from './client'

export interface ThemePackInfo {
  id: string
  name: string
  author?: string
  description?: string
  version?: string
  installed_at: string
  tokens_url: string
  assets_url: string
}

export async function listThemePacks(): Promise<ThemePackInfo[]> {
  const res = await client.get<{ success: number; data: ThemePackInfo[] }>(
    '/v1/sys/themes',
  )
  return res.data.data ?? []
}

export async function uploadThemePack(file: File): Promise<ThemePackInfo> {
  const form = new FormData()
  form.append('file', file)
  const res = await client.post<{ success: number; data: ThemePackInfo }>(
    '/v1/sys/themes',
    form,
    {
      headers: { 'Content-Type': 'multipart/form-data' },
      // Theme packs can be tens of MB — the client-wide 10s default killed
      // slow-link uploads mid-flight (same bug Files had).
      timeout: 10 * 60_000,
    },
  )
  return res.data.data
}

export async function deleteThemePack(id: string): Promise<void> {
  await client.delete(
    `/v1/sys/themes/${encodeURIComponent(id)}`,
  )
}
