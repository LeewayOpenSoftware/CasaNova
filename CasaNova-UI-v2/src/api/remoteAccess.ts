// Remote-access connector status — inventories which of Tailscale,
// Cloudflare Tunnel, and ZeroTier are installed and running on the
// server so the Settings → Remote Access page can render a unified
// view.

import client from './client'

export interface ConnectorStatus {
  id: 'tailscale' | 'cloudflared' | 'zerotier'
  name: string
  installed: boolean
  running: boolean
  detail: string
  install_url: string
}

export async function getRemoteAccessStatus(): Promise<ConnectorStatus[]> {
  const res = await client.get<{ success: number; data: ConnectorStatus[] }>(
    '/v1/sys/remote-access/status',
  )
  return res.data.data
}
