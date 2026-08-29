// Notifications fixture for the Stage 5.1 notification center.
//
// Shape matches the CasaNova-Common notifications.Notification struct
// (and src/api/notifications.ts) so the swap to the real API is a
// one-line change — the store's refresh() tries the real endpoint and
// falls back to this fixture when unreachable (dev scaffold, no backend
// running).
//
// The fixture covers every level (info / success / warning / error) and
// every category (app / storage / system / update / security / backup)
// so the category tab UI + filter chips have something meaningful to
// show against.

import type { Notification } from '@/api/notifications'

// Compute timestamps relative to "now" so dayjs.fromNow() renders as
// "5 minutes ago", "2 hours ago", etc. instead of static absolute dates
// that go stale the moment the dev server has been running for a bit.
const now = Date.now()
const minutes = (n: number) => new Date(now - n * 60_000).toISOString()
const hours = (n: number) => new Date(now - n * 3_600_000).toISOString()
const days = (n: number) => new Date(now - n * 86_400_000).toISOString()

export const notificationsFixture: Notification[] = [
  {
    id: 1,
    level: 'warning',
    category: 'security',
    title: 'Rate limit triggered',
    message: 'IP 203.0.113.14 was temporarily banned (4 hour strike) after exceeding the request budget.',
    read: false,
    created_at: minutes(4),
    link: '/app/settings/security',
    link_label: 'Review bans',
  },
  {
    id: 2,
    level: 'success',
    category: 'backup',
    title: 'Nextcloud backup completed',
    message: 'Restic snapshot created. 34 snapshots retained, 48.2 GB total.',
    read: false,
    created_at: minutes(12),
    link: '/app/settings/backups',
    link_label: 'View snapshots',
  },
  {
    id: 3,
    level: 'error',
    category: 'app',
    title: 'Container exited unexpectedly',
    message: 'jellyfin exited with status 137 (OOM). Consider raising the memory limit.',
    read: false,
    created_at: minutes(45),
    link: '/app/settings/containers',
  },
  {
    id: 4,
    level: 'warning',
    category: 'storage',
    title: 'SMART health degraded',
    message: '/dev/sda reports 3 reallocated sectors (up from 0). Keep an eye on this disk.',
    read: false,
    created_at: hours(3),
    link: '/app/settings/storage',
  },
  {
    id: 5,
    level: 'info',
    category: 'update',
    title: 'CasaNova v0.9.1 available',
    message: 'A new release is ready to install. Changelog includes 12 bug fixes and 2 new features.',
    read: true,
    created_at: hours(8),
  },
  {
    id: 6,
    level: 'info',
    category: 'system',
    title: 'Hostname changed',
    message: 'System hostname was updated to "casanova".',
    read: true,
    created_at: days(1),
  },
  {
    id: 7,
    level: 'success',
    category: 'app',
    title: 'App installed',
    message: 'Immich was installed successfully from the app store.',
    read: true,
    created_at: days(2),
  },
  {
    id: 8,
    level: 'info',
    category: 'security',
    title: 'New login',
    message: 'User ana signed in from 192.168.1.77 (Safari on iPhone).',
    read: true,
    created_at: days(2),
  },
  {
    id: 9,
    level: 'warning',
    category: 'system',
    title: 'TLS certificate expires soon',
    message: 'Certificate for home.example.com expires in 30 days. Let\'s Encrypt will attempt automatic renewal.',
    read: true,
    created_at: days(3),
  },
]

export function fixtureListResult(params: { limit?: number } = {}) {
  const limit = params.limit ?? 50
  const sliced = notificationsFixture.slice(0, limit)
  return {
    notifications: sliced,
    unread_count: notificationsFixture.filter((n) => !n.read).length,
  }
}
