// Notification center API.
//
// Thin typed wrapper around /v1/sys/notifications — the HTTP endpoints we
// added in Wave 3b/3e on the CasaNova backend. The shape matches the
// notifications.Notification struct in CasaNova-Common/utils/notifications.
//
// We keep the module tiny on purpose: four methods map 1:1 to four backend
// endpoints, and the Pinia store in @/stores/notifications is what the UI
// actually talks to. Keeping the HTTP layer separate makes future swaps
// (WebSocket push, server-sent events) cheap — only the store changes.

import client from './client'

export type NotificationLevel = 'info' | 'success' | 'warning' | 'error'
export type NotificationCategory =
  | 'app'
  | 'storage'
  | 'system'
  | 'update'
  | 'security'
  | 'backup'

export interface Notification {
  id: number
  level: NotificationLevel
  category: NotificationCategory
  title: string
  message: string
  link?: string
  link_label?: string
  read: boolean
  created_at: string
}

export interface ListResult {
  notifications: Notification[]
  unread_count: number
}

/**
 * List notifications, optionally filtered.
 * Maps to GET /v1/sys/notifications?unread=&category=&level=&limit=&offset=
 */
export async function list(params: {
  unread?: boolean
  category?: NotificationCategory
  level?: NotificationLevel
  limit?: number
  offset?: number
}): Promise<ListResult> {
  const res = await client.get<{ success: number; data: ListResult }>(
    '/v1/sys/notifications',
    {
      params: {
        unread: params.unread ? 'true' : undefined,
        category: params.category,
        level: params.level,
        limit: params.limit,
        offset: params.offset,
      },
    },
  )
  return res.data.data
}

/**
 * Mark one or more notifications as read.
 * Passing `all: true` marks every notification read regardless of IDs.
 * Maps to POST /v1/sys/notifications/read
 */
export async function markRead(body: { ids?: number[]; all?: boolean }): Promise<void> {
  await client.post('/v1/sys/notifications/read', body)
}

/**
 * Delete a single notification by ID.
 * Maps to DELETE /v1/sys/notifications/:id
 */
export async function remove(id: number): Promise<void> {
  await client.delete(`/v1/sys/notifications/${id}`)
}
