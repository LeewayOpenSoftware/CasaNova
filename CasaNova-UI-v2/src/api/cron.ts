// Cron API — wraps /v1/sys/cron/* endpoints.
//
// Thin typed wrappers around the cron job management backend.
// The ScheduledSettings view calls these via safeCall() for fixture
// fallback during development.

import client from './client'

export interface CronJob {
  id: string
  schedule: string
  command: string
  description: string
  enabled: boolean
}

export async function getJobs(): Promise<CronJob[]> {
  const res = await client.get<{ success: number; data: CronJob[] }>(
    '/v1/sys/cron',
  )
  return res.data.data
}

export async function createJob(
  job: Pick<CronJob, 'schedule' | 'command' | 'description'>,
): Promise<CronJob> {
  const res = await client.post<{ success: number; data: CronJob }>(
    '/v1/sys/cron',
    job,
  )
  return res.data.data
}

export async function updateJob(
  id: string,
  job: Pick<CronJob, 'schedule' | 'command' | 'description'>,
): Promise<CronJob> {
  const res = await client.put<{ success: number; data: CronJob }>(
    `/v1/sys/cron/${encodeURIComponent(id)}`,
    job,
  )
  return res.data.data
}

export async function deleteJob(id: string): Promise<void> {
  await client.delete(`/v1/sys/cron/${encodeURIComponent(id)}`)
}

export async function toggleJob(id: string): Promise<CronJob> {
  const res = await client.post<{ success: number; data: CronJob }>(
    `/v1/sys/cron/${encodeURIComponent(id)}/toggle`,
  )
  return res.data.data
}
