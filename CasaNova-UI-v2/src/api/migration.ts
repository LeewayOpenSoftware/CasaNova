// Migration assistant — read-only report of CasaOS → CasaNova state
// + an acknowledgement endpoint that marks the migration "complete"
// so the dashboard YSK card disappears.

import client from './client'

export interface MigrationConfig {
  name: string
  path: string
  size: number // bytes; -1 if not regular file
}

export interface MigrationLegacyUnit {
  name: string
  active: boolean
  enabled: boolean
}

export interface MigrationReport {
  legacy_config_dir: boolean
  configs_migrated: MigrationConfig[]
  legacy_compose_apps_dir: string
  legacy_compose_apps: string[]
  legacy_containers: number
  legacy_units: MigrationLegacyUnit[]
  acknowledged: boolean
  generated_at: string
}

export async function getMigrationReport(): Promise<MigrationReport> {
  const res = await client.get<{ success: number; data: MigrationReport }>(
    '/v1/sys/migration/report',
  )
  return res.data.data
}

export async function acknowledgeMigration(): Promise<void> {
  await client.post('/v1/sys/migration/acknowledge')
}
