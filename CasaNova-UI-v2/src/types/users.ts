// User types extracted from fixtures/users.ts
// so production code can import these without pulling in fake data.

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
