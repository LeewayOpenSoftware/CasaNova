// Alerts API — wraps /v1/sys/alerts/* endpoints.
//
// Thin typed wrappers around the alert threshold rules backend.
// The AlertsSettings view calls these via safeCall() for fixture
// fallback during development.

import client from './client'

export interface AlertRule {
  id: string
  metric: string
  operator: string
  threshold: number
  duration_seconds: number
  enabled: boolean
  notify: string
  label: string
}

export interface AlertEvent {
  rule_id: string
  triggered_at: string
  value: number
  resolved_at?: string
}

export async function getRules(): Promise<AlertRule[]> {
  const res = await client.get<{ success: number; data: AlertRule[] }>(
    '/v1/sys/alerts/rules',
  )
  return res.data.data
}

export async function createRule(rule: Omit<AlertRule, 'id'>): Promise<AlertRule> {
  const res = await client.post<{ success: number; data: AlertRule }>(
    '/v1/sys/alerts/rules',
    rule,
  )
  return res.data.data
}

export async function updateRule(id: string, rule: Omit<AlertRule, 'id'>): Promise<AlertRule> {
  const res = await client.put<{ success: number; data: AlertRule }>(
    `/v1/sys/alerts/rules/${encodeURIComponent(id)}`,
    rule,
  )
  return res.data.data
}

export async function deleteRule(id: string): Promise<void> {
  await client.delete(`/v1/sys/alerts/rules/${encodeURIComponent(id)}`)
}

export async function getHistory(since?: string): Promise<AlertEvent[]> {
  const res = await client.get<{ success: number; data: AlertEvent[] }>(
    '/v1/sys/alerts/history',
    { params: since ? { since } : undefined },
  )
  return res.data.data
}

export async function testRule(id: string): Promise<AlertEvent> {
  const res = await client.post<{ success: number; data: AlertEvent }>(
    `/v1/sys/alerts/test/${encodeURIComponent(id)}`,
  )
  return res.data.data
}
