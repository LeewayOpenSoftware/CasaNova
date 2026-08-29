// Mock audit events for Stage 5.2 Audit Log development.
//
// Shape matches the CasaNova-Common audit store rows (v0.9): id,
// timestamp, severity, category, actor, ip, message, metadata.
// Severities and categories mirror the real backend enums so the
// filter UI built against this fixture keeps working unchanged when
// wired to GET /v1/sys/audit.

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

export const auditFixture: AuditEvent[] = [
  {
    id: 'a-1',
    timestamp: '2026-04-05T08:12:00Z',
    severity: 'info',
    category: 'auth',
    actor: 'nicolas',
    ip: '192.168.1.42',
    message: 'User logged in successfully',
  },
  {
    id: 'a-2',
    timestamp: '2026-04-05T07:58:00Z',
    severity: 'warning',
    category: 'security',
    actor: 'shield',
    ip: '203.0.113.14',
    message: 'Rate limit threshold crossed — 1st strike (1 hour ban)',
    metadata: { tier: 'strike', requests: 18342 },
  },
  {
    id: 'a-3',
    timestamp: '2026-04-05T03:00:00Z',
    severity: 'info',
    category: 'backup',
    actor: 'system',
    ip: '127.0.0.1',
    message: 'Restic backup completed for app "nextcloud"',
    metadata: { app: 'nextcloud', snapshots: 1, size: '48.2 GB' },
  },
  {
    id: 'a-4',
    timestamp: '2026-04-04T22:45:00Z',
    severity: 'info',
    category: 'auth',
    actor: 'ana',
    ip: '192.168.1.77',
    message: 'User logged in successfully',
  },
  {
    id: 'a-5',
    timestamp: '2026-04-04T18:14:00Z',
    severity: 'error',
    category: 'app',
    actor: 'nicolas',
    ip: '192.168.1.42',
    message: 'Container "jellyfin" exited with status 137 (OOM)',
    metadata: { container: 'jellyfin', exitCode: 137 },
  },
  {
    id: 'a-6',
    timestamp: '2026-04-04T15:30:00Z',
    severity: 'info',
    category: 'app',
    actor: 'nicolas',
    ip: '192.168.1.42',
    message: 'Installed app "immich" from app store',
  },
  {
    id: 'a-7',
    timestamp: '2026-04-03T11:22:00Z',
    severity: 'warning',
    category: 'storage',
    actor: 'system',
    ip: '127.0.0.1',
    message: 'Disk /dev/sdb SMART: 3 reallocated sectors (up from 0)',
    metadata: { device: '/dev/sdb', reallocated: 3 },
  },
  {
    id: 'a-8',
    timestamp: '2026-04-02T09:00:00Z',
    severity: 'info',
    category: 'network',
    actor: 'nicolas',
    ip: '192.168.1.42',
    message: 'Gateway TLS renewed via Let\'s Encrypt',
    metadata: { domain: 'home.example.com', expires: '2026-07-01' },
  },
  {
    id: 'a-9',
    timestamp: '2026-04-01T20:15:00Z',
    severity: 'critical',
    category: 'security',
    actor: 'shield',
    ip: '198.51.100.23',
    message: 'IP permanently banned after 3rd rate-limit violation',
    metadata: { tier: 'permanent' },
  },
  {
    id: 'a-10',
    timestamp: '2026-03-30T14:00:00Z',
    severity: 'info',
    category: 'system',
    actor: 'nicolas',
    ip: '192.168.1.42',
    message: 'Hostname updated to "casanova"',
  },
]
