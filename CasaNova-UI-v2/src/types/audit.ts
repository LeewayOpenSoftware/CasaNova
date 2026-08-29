// Audit types extracted from fixtures/audit.ts
// so production code can import these without pulling in fake data.

export type AuditSeverity = 'info' | 'warning' | 'error' | 'critical'
export type AuditCategory = 'auth' | 'app' | 'storage' | 'network' | 'system' | 'security' | 'backup'

export interface AuditEvent {
  id: string
  timestamp: string // ISO 8601
  severity: AuditSeverity
  category: AuditCategory
  actor: string
  ip: string
  message: string
  metadata?: Record<string, unknown>
}
