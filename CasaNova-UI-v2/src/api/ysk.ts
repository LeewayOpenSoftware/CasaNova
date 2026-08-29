// YSK ("You Should Know") cards — onboarding + at-a-glance information
// cards rendered on the dashboard. Backend lives in CasaNova-MessageBus
// and supports:
//
//   GET    /v2/message_bus/ysk          → list active cards
//   DELETE /v2/message_bus/ysk/{id}     → dismiss a card (permanent)
//
// Cards have a rich body — progress bar, icon+text, or list of items.
// Footer actions can dispatch back to the bus (left/right buttons that
// fire an event like "open this app" or "go to settings"), but for v0.1
// we treat actions as opaque payloads forwarded to the bus action API.
//
// Bus push: when a card is upserted or deleted on the server, an event
// (`ysk:card:upsert` / `ysk:card:delete`) is broadcast over the
// MessageBus WebSocket. The store layer subscribes to those and
// invalidates so the UI re-fetches without polling.

import client from './client'

// ---------- Types ----------

export type YSKCardIcon = string // free-form (URL or lucide-name); backend doesn't validate

export type YSKActionStyle = 'primary' | 'secondary' | 'text'
export type YSKActionSide = 'left' | 'right'

export interface YSKCardProgress {
  label: string
  progress: number // 0-100
}

export interface YSKCardIconWithText {
  icon: YSKCardIcon
  description: string
}

export interface YSKCardListItem {
  icon: YSKCardIcon
  description: string
  rightText: string
}

export interface YSKMessageBusAction {
  // Forwarded to the backend's action API verbatim. Backend interprets
  // sourceID + name to decide what to do (open app, navigate, etc).
  sourceID?: string
  name?: string
  properties?: Record<string, string>
}

export interface YSKCardFooterAction {
  side: YSKActionSide
  style: YSKActionStyle
  text: string
  messageBus: YSKMessageBusAction
}

export interface YSKCardContent {
  titleIcon: YSKCardIcon
  titleText: string
  bodyProgress?: YSKCardProgress
  bodyIconWithText?: YSKCardIconWithText
  bodyList?: YSKCardListItem[]
  footerActions?: YSKCardFooterAction[]
}

export interface YSKCard {
  id: string
  cardType: string
  renderType: string
  content: YSKCardContent
}

// ---------- Client ----------

const YSK_BASE = '/v2/message_bus/ysk'

interface YSKListEnvelope {
  data?: YSKCard[]
  message?: string
}

export async function listYskCards(): Promise<YSKCard[]> {
  const res = await client.get<YSKListEnvelope>(YSK_BASE)
  return res.data?.data ?? []
}

export async function dismissYskCard(id: string): Promise<void> {
  // Path traversal guard mirrors the backend's input validation in
  // CasaNova-MessageBus/route/ysk.go — the backend will return 400 on
  // a bad id, but rejecting client-side avoids a wasted round trip and
  // a confusing error in the dashboard's notification toast.
  if (!id || id.includes('/') || id.includes('\\')) {
    throw new Error(`invalid YSK card id: ${id}`)
  }
  await client.delete(`${YSK_BASE}/${encodeURIComponent(id)}`)
}
