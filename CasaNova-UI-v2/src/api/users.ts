// User-service endpoints exposed as a typed module.
//
// Each function is a thin wrapper around a REST call with TypeScript types
// on both sides. This file will eventually be generated from the UserService
// OpenAPI spec via openapi-typescript, but for the Stage 1 vertical slice we
// hand-type the two endpoints Login needs and leave the rest for later.

import client from './client'

export interface LoginRequest {
  username: string
  password: string
}

export interface LoginTokens {
  access_token: string
  refresh_token: string
  expires_at: number
}

export interface LoginUser {
  id: number
  username: string
  role: string
  nickname?: string
  email?: string
  avatar?: string
}

export interface LoginResponse {
  success: number
  message: string
  data: {
    token: LoginTokens
    user: LoginUser
  }
}

export async function login(body: LoginRequest): Promise<LoginResponse> {
  try {
    const res = await client.post<LoginResponse>('/v1/users/login', body)
    return res.data
  } catch (err) {
    // Dev-scaffold fallback: when the real backend is unreachable, mint
    // a local session so the UI is usable without a running server. We
    // accept any non-empty credentials — the dev scaffold has no
    // password database to validate against. In production this branch
    // never fires because the backend is up and returns a real token.
    //
    // Detection covers both direct network errors (ERR_NETWORK when
    // there's no proxy or the proxy refuses) and Vite proxy 5xx
    // responses (the proxy returns 500/502/504 when it can't reach
    // the upstream CasaNova server).
    const e = err as {
      code?: string
      message?: string
      response?: { status?: number; data?: unknown }
    }
    const isNetworkError =
      e?.code === 'ERR_NETWORK' ||
      e?.message?.includes('Network Error') ||
      (typeof e?.response?.status === 'number' && e.response.status >= 500)
    if (!isNetworkError) throw err
    if (!import.meta.env.DEV) throw err // Only allow dev fallback in development builds
    if (!body.username.trim() || !body.password) throw err
    const now = Math.floor(Date.now() / 1000)
    return {
      success: 200,
      message: 'ok (dev fallback)',
      data: {
        token: {
          access_token: 'dev-' + now,
          refresh_token: 'dev-refresh-' + now,
          expires_at: now + 60 * 60 * 24,
        },
        user: {
          id: 1,
          username: body.username.trim(),
          role: 'admin',
          nickname: body.username.trim(),
        },
      },
    }
  }
}

export interface UserStatus {
  initialized: boolean
  // Wave 28: legacy field, kept optional so older backends still
  // type-check. New backends always return key as "" and instead
  // provide key_file pointing at the on-disk bootstrap key.
  key?: string
  key_file?: string
  gpus?: number
}

export interface UserStatusResponse {
  success: number
  message: string
  data: UserStatus
}

export async function getUserStatus(): Promise<UserStatusResponse> {
  const res = await client.get<UserStatusResponse>('/v1/users/status')
  return res.data
}

export interface RegisterRequest {
  username: string
  password: string
  key: string
}

export interface RegisterResponse {
  success: number
  message: string
}

export async function register(body: RegisterRequest): Promise<RegisterResponse> {
  const res = await client.post<RegisterResponse>('/v1/users/register', body)
  return res.data
}

// ─────────────────────────────────────────────────────────────────────
// Per-user UI preference blob
//
// Wave 29: the UI used to write theme, app positions, dashboard tweaks
// etc. straight to localStorage, which made every preference a per-
// browser setting. Logging in from a different machine — or even a
// different browser on the same machine — would expose the defaults,
// surprising users who expected their dashboard to follow them.
//
// We piggy-back on the existing /v1/users/current/custom/:key endpoint
// (UserService writes the body to <UserDataPath>/<user_id>/<key>.json),
// using the well-known key "ui-prefs" for the entire serialized blob.
// No new backend code is required — only this client wrapper, plus a
// frontend cache that hydrates on login and flushes on change.
//
// The blob is intentionally a flat string→string map so we can keep
// using the existing localStorage keys verbatim as a warm cache for
// hydration latency and offline use.
// ─────────────────────────────────────────────────────────────────────

export const UI_PREFS_KEY = 'ui-prefs'

export interface UiPrefsBlob {
  // Bumped if we ever change the storage layout. Old blobs whose
  // version we don't know how to read are treated as empty (the user
  // sees defaults; their next change overwrites the unreadable blob).
  version: number
  // Map of original localStorage keys → string values. Keeping the
  // shape flat means individual stores don't need to know whether they
  // hit local cache or backend — they just call getPref / setPref.
  values: Record<string, string>
}

interface CustomConfWrapper<T> {
  success: number
  message: string
  data: T | string
}

/**
 * Fetch the current user's UI preference blob. Returns null when the
 * server has nothing yet (first-time user) or when the body fails to
 * parse — the caller treats both as "fall back to defaults".
 *
 * Network or auth errors propagate so the caller can decide whether to
 * retry or fall back to localStorage.
 */
export async function getUiPrefs(): Promise<UiPrefsBlob | null> {
  const res = await client.get<CustomConfWrapper<UiPrefsBlob>>(
    `/v1/users/current/custom/${UI_PREFS_KEY}`,
  )
  const data = res.data?.data
  if (!data) return null
  // The backend returns a JSON object directly when the file is valid
  // JSON, otherwise the raw string. We only accept the object form
  // because anything else means the file was hand-edited or corrupted.
  if (typeof data === 'string') {
    if (!data.trim()) return null
    try {
      const parsed = JSON.parse(data) as UiPrefsBlob
      if (parsed && parsed.version === 1 && parsed.values) return parsed
    } catch {
      return null
    }
    return null
  }
  if ((data as UiPrefsBlob).version === 1 && (data as UiPrefsBlob).values) {
    return data as UiPrefsBlob
  }
  return null
}

/**
 * Persist the current user's UI preference blob. The endpoint accepts
 * the raw body and writes it under <UserDataPath>/<user_id>/ui-prefs.json
 * — there's no diff/patch protocol, so callers MUST send the full blob.
 */
export async function putUiPrefs(blob: UiPrefsBlob): Promise<void> {
  await client.post(`/v1/users/current/custom/${UI_PREFS_KEY}`, blob)
}

// ─────────────────────────────────────────────────────────────────────
// Avatar upload. The UserService handler (PUT /v1/users/avatar) strips the
// literal prefix "data:image/png;base64," and only has the PNG decoder
// registered, so `dataUrl` MUST be a PNG data URL. It caps the base64 body
// at 10 MB and rejects images larger than 4096x4096. Callers use
// fileToPngDataUrl() below to re-encode + bound the picked file first.
// ─────────────────────────────────────────────────────────────────────
export interface UpdateAvatarResponse {
  success: number
  message: string
  data?: LoginUser
}

export async function updateAvatar(dataUrl: string): Promise<UpdateAvatarResponse> {
  const res = await client.put<UpdateAvatarResponse>('/v1/users/avatar', { file: dataUrl })
  return res.data
}

// Change the current user's password. The backend (PUT /v1/users/current/password)
// verifies old_password and revokes all refresh-token families on success, so a
// stolen session is cut off. Throws on a non-2xx (e.g. wrong old password).
export async function updatePassword(oldPassword: string, newPassword: string): Promise<void> {
  await client.put('/v1/users/current/password', { old_password: oldPassword, password: newPassword })
}

// Re-encode any picked image to a PNG data URL bounded to `max` px on the
// long edge. Guarantees the exact "data:image/png;base64," prefix the
// backend strips verbatim, keeps well under the 10 MB / 4096px limits, and
// lets JPEG/WebP/GIF sources work even though the backend decodes PNG only.
export function fileToPngDataUrl(file: File, max = 512): Promise<string> {
  return new Promise((resolve, reject) => {
    const img = new Image()
    const url = URL.createObjectURL(file)
    img.onload = () => {
      URL.revokeObjectURL(url)
      const scale = Math.min(1, max / Math.max(img.width, img.height))
      const w = Math.max(1, Math.round(img.width * scale))
      const h = Math.max(1, Math.round(img.height * scale))
      const canvas = document.createElement('canvas')
      canvas.width = w
      canvas.height = h
      const ctx = canvas.getContext('2d')
      if (!ctx) return reject(new Error('no 2d context'))
      ctx.drawImage(img, 0, 0, w, h)
      resolve(canvas.toDataURL('image/png'))
    }
    img.onerror = () => {
      URL.revokeObjectURL(url)
      reject(new Error('image decode failed'))
    }
    img.src = url
  })
}
