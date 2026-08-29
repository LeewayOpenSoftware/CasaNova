// Mock users fixture for Stage 5.3 Users & Roles development.
//
// Shape matches the CasaNova-UserService v0.9 RBAC response. The
// backend ships a 17-bit permission mask per role; the UI only needs
// to display the role + the top-level capabilities for now, so the
// fixture summarizes permissions as human-readable strings. The real
// data source will be GET /v1/users + GET /v1/users/roles.

export type UserRole = 'admin' | 'user' | 'guest'

export interface UserEntry {
  id: string
  username: string
  role: UserRole
  avatarUrl?: string
  createdAt: string // ISO 8601
  lastLoginAt: string | null
  email?: string
}

export const rolesFixture: Record<UserRole, { label: string; permissionCount: number; description: string }> = {
  admin: {
    label: 'Administrator',
    permissionCount: 17,
    description: 'Full access: users, apps, storage, network, security, backups.',
  },
  user: {
    label: 'User',
    permissionCount: 9,
    description: 'Install & manage their own apps, browse files, run terminals.',
  },
  guest: {
    label: 'Guest',
    permissionCount: 3,
    description: 'Read-only dashboard and limited app list; no settings access.',
  },
}

export const usersFixture: UserEntry[] = [
  {
    id: 'u-1',
    username: 'nicolas',
    role: 'admin',
    createdAt: '2025-08-14T09:00:00Z',
    lastLoginAt: '2026-04-05T08:12:00Z',
    email: 'nicolas@lucentopen.dev',
  },
  {
    id: 'u-2',
    username: 'ana',
    role: 'user',
    createdAt: '2026-01-22T13:30:00Z',
    lastLoginAt: '2026-04-04T22:45:00Z',
    email: 'ana@example.com',
  },
  {
    id: 'u-3',
    username: 'guest',
    role: 'guest',
    createdAt: '2026-03-01T10:00:00Z',
    lastLoginAt: '2026-03-28T17:20:00Z',
  },
]
