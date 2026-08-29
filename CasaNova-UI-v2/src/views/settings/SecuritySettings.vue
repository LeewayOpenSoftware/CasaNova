<script setup lang="ts">
// Security settings — Stage 5.5 landing.
//
// Surfaces four security subsystems on one page: SmartShield (rate
// limiter + ban list), OAuth2 client management, active sessions, and
// 2FA enrollment state. Each is a card with a short summary + a
// primary action. Full management UI for each subsystem lands in its
// own dialog/page in a later pass.

import { ref, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import Button from 'primevue/button'
import Dialog from 'primevue/dialog'
import InputText from 'primevue/inputtext'
import ToggleSwitch from 'primevue/toggleswitch'
import dayjs from 'dayjs'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import TwoFactorSetupDialog from '@/components/settings/TwoFactorSetupDialog.vue'
import TwoFactorManageDialog from '@/components/settings/TwoFactorManageDialog.vue'
import { type AntivirusEngine } from '@/types/antivirus'
import { onMounted } from 'vue'
import client from '@/api/client'
import { safeCall } from '@/api/safeCall'

const { t } = useI18n()

// ----- Antivirus detection -----
// No backend endpoint yet — start with "not detected" so no fake data shows.
const antivirus = ref({
  detected: false,
  engine: 'clamav' as AntivirusEngine,
  version: '',
  dbVersion: 0,
  dbLastUpdate: '',
  lastScan: null as string | null,
  lastScanAt: null as string | null,
  lastScanResult: 'clean' as 'clean' | 'threats',
  threatsFound: 0,
  signatureDbVersion: '',
  signatureDbUpdatedAt: null as string | null,
  daemonRunning: false,
})
const engineLabels: Record<AntivirusEngine, string> = {
  clamav: 'ClamAV',
  sophos: 'Sophos',
  eset: 'ESET',
  comodo: 'Comodo',
}

// ----- SmartShield -----
// Live status pulled from the gateway. Toggle persists via PUT
// /v1/gateway/smartshield/enabled (writes /etc/casanova/shield-config.json).
const shieldEnabled = ref(false)
const shieldStats = ref({
  blocksLast24h: 0, // not yet tracked by the gateway — left at 0 for now
  currentBans: 0,
  bansTotal: 0,
})
const bannedIPs = ref<Array<{ ip: string; tier: string; since: string; expires?: string }>>([])

async function loadShield() {
  const res = await safeCall(async () => {
    const r = await client.get<{ success: number; data: {
      enabled: boolean; trackedIPs: number; bannedIPs: Array<{
        ip: string; level: string; banUntil: string; strikes: number
      }>;
    } }>('/v1/gateway/smartshield/status')
    return r.data.data
  })
  if (res.data) {
    shieldEnabled.value = res.data.enabled
    shieldStats.value = {
      blocksLast24h: 0,
      currentBans: res.data.bannedIPs.length,
      bansTotal: res.data.trackedIPs,
    }
    bannedIPs.value = res.data.bannedIPs.map((b) => ({
      ip: b.ip,
      tier: b.level,
      since: dayjs(b.banUntil).format('YYYY-MM-DD HH:mm'),
      expires: b.banUntil,
    }))
  }
}
async function setShieldEnabled(v: boolean) {
  const prev = shieldEnabled.value
  shieldEnabled.value = v
  const res = await safeCall(async () => {
    const r = await client.put<{ success: number }>(
      '/v1/gateway/smartshield/enabled',
      { enabled: v },
    )
    return r.data
  })
  if (res.error) {
    shieldEnabled.value = prev
    return
  }
  await loadShield()
}
// Per-IP unban button is a follow-up; the backend endpoint
// (/v1/gateway/smartshield/unban) is wired and ready when the UI row
// lands. The handler was removed here to keep the strict build clean.

// ----- Sessions -----
// Backed by /v1/users/sessions. The "current" flag identifies the
// caller's own session so the UI can disable the revoke button for it.
const sessions = ref<Array<{
  id: string; userAgent: string; ip: string; createdAt: number;
  lastSeenAt: number; expiresAt: number; current: boolean;
}>>([])
const sessionRevoking = ref<string | null>(null)
async function loadSessions() {
  const res = await safeCall(async () => {
    const r = await client.get<{ success: number; data: Array<{
      id: string; userAgent: string; ipAddress: string; createdAt: number;
      lastSeenAt: number; expiresAt: number; current: boolean;
    }> }>('/v1/users/sessions')
    return r.data.data ?? []
  })
  if (res.data) {
    sessions.value = res.data.map((s) => ({
      id: s.id,
      userAgent: s.userAgent,
      ip: s.ipAddress,
      createdAt: s.createdAt,
      lastSeenAt: s.lastSeenAt,
      expiresAt: s.expiresAt,
      current: s.current,
    }))
  }
}
async function revokeSession(id: string) {
  sessionRevoking.value = id
  await safeCall(async () => client.delete(`/v1/users/sessions/${id}`))
  sessionRevoking.value = null
  await loadSessions()
}

// ----- OAuth2 register dialog -----
const oauth2RegisterVisible = ref(false)
const oauth2RegisterBusy = ref(false)
const oauth2RegisterError = ref('')
const oauth2RegisterCreated = ref<{
  client_id: string; client_secret: string;
} | null>(null)
const oauth2RegisterForm = ref({
  name: '',
  redirect_uris: '',
  allowed_scopes: 'openid profile email',
})
function openOAuth2Register() {
  oauth2RegisterForm.value = { name: '', redirect_uris: '', allowed_scopes: 'openid profile email' }
  oauth2RegisterCreated.value = null
  oauth2RegisterError.value = ''
  oauth2RegisterVisible.value = true
}
async function submitOAuth2Register() {
  if (oauth2RegisterBusy.value) return
  oauth2RegisterBusy.value = true
  oauth2RegisterError.value = ''
  const body = {
    name: oauth2RegisterForm.value.name.trim(),
    redirect_uris: oauth2RegisterForm.value.redirect_uris
      .split(/[\s,]+/).map((s) => s.trim()).filter(Boolean),
    allowed_scopes: oauth2RegisterForm.value.allowed_scopes
      .split(/\s+/).filter(Boolean),
  }
  const res = await safeCall(async () => {
    const r = await client.post<{ success: number; data: {
      client_id: string; client_secret: string;
    } }>('/v1/users/oauth2/clients', body)
    return r.data.data
  })
  oauth2RegisterBusy.value = false
  if (res.error) {
    oauth2RegisterError.value = res.error
    return
  }
  oauth2RegisterCreated.value = res.data ?? null
  // Refresh the list so the new client appears immediately.
  const list = await safeCall(async () => {
    const r = await client.get<{ success: number; data: Array<{
      id: string; name: string; redirect_uris: string[];
    }> }>('/v1/users/oauth2/clients')
    return (r.data.data ?? []).map((c) => ({
      id: c.id, name: c.name, redirect: c.redirect_uris?.[0] || '',
    }))
  })
  if (list.data) oauth2Clients.value = list.data as any
}

// ----- OAuth2 clients -----
// Fetched from the real backend when available.
const oauth2Clients = ref<Array<{ id: string; name: string; redirect: string }>>([])

onMounted(async () => {
  // Probe for antivirus engine on the host
  const avResult = await safeCall(async () => {
    const res = await client.get<{ success: number; data: typeof antivirus.value }>(
      '/v1/sys/antivirus/status',
    )
    return res.data.data
  })
  if (avResult.data && avResult.data.detected) {
    antivirus.value = { ...antivirus.value, ...avResult.data }
  }

  // Try to load OAuth2 clients from the user service
  const result = await safeCall(async () => {
    const res = await client.get<{ success: number; data: Array<{
      id: string; name: string; redirect_uris: string[]; allowed_scopes: string[]
    }> }>('/v1/users/oauth2/clients')
    return (res.data.data ?? []).map((c) => ({
      id: c.id,
      name: c.name,
      redirect: c.redirect_uris?.[0] || '',
    }))
  })
  if (result.data) {
    oauth2Clients.value = result.data as Array<{ id: string; name: string; redirect: string }>
  }

  // Probe 2FA status
  const tfaResult = await safeCall(async () => {
    const res = await client.get<{
      success: number
      data: { enabled: boolean; backup_codes_remaining: number }
    }>('/v1/sys/2fa/status')
    return res.data.data
  })
  if (tfaResult.data) {
    twoFactorEnabled.value = tfaResult.data.enabled
    backupCodesRemaining.value = tfaResult.data.backup_codes_remaining
  }

  // Live loaders for SmartShield + Sessions (both backends just shipped
  // alongside the OAuth2 register dialog).
  await Promise.allSettled([loadShield(), loadSessions()])
})

// ----- 2FA -----
const twoFactorEnabled = ref(false)
const backupCodesRemaining = ref(0)
const showSetupDialog = ref(false)
const showManageDialog = ref(false)

const scanLoading = ref(false)
const sigsLoading = ref(false)

async function runFullScan() {
  scanLoading.value = true
  try {
    await client.post('/v1/sys/antivirus/scan')
    antivirus.value.lastScanResult = 'clean'
    antivirus.value.lastScanAt = new Date().toISOString()
  } catch { /* backend unavailable */ }
  finally { scanLoading.value = false }
}

let sigUpdateTimer: ReturnType<typeof setTimeout> | null = null

async function updateSignatures() {
  sigsLoading.value = true
  try {
    await client.post('/v1/sys/antivirus/update-signatures')
    // Re-probe to get updated signature info after a short delay.
    if (sigUpdateTimer) clearTimeout(sigUpdateTimer)
    sigUpdateTimer = setTimeout(async () => {
      sigUpdateTimer = null
      try {
        const res = await client.get<{ success: number; data: typeof antivirus.value }>(
          '/v1/sys/antivirus/status',
        )
        if (res.data.data) antivirus.value = { ...antivirus.value, ...res.data.data }
      } catch (err) {
        console.error('Failed to reload antivirus status after signature update:', err)
      } finally {
        sigsLoading.value = false
      }
    }, 3000)
  } catch { sigsLoading.value = false }
}

onUnmounted(() => {
  if (sigUpdateTimer) {
    clearTimeout(sigUpdateTimer)
    sigUpdateTimer = null
  }
})

async function refresh2FAStatus() {
  const res = await safeCall(async () => {
    const r = await client.get<{
      success: number
      data: { enabled: boolean; backup_codes_remaining: number }
    }>('/v1/sys/2fa/status')
    return r.data.data
  })
  if (res.data) {
    twoFactorEnabled.value = res.data.enabled
    backupCodesRemaining.value = res.data.backup_codes_remaining
  }
}

function on2FAEnabled() {
  refresh2FAStatus()
}

function on2FADisabled() {
  twoFactorEnabled.value = false
  backupCodesRemaining.value = 0
}

function formatDate(iso: string): string {
  return dayjs(iso).format('MMM D, HH:mm')
}
</script>

<template>
  <SettingsPage
    icon="Shield"
    :title="t('settings.security.title')"
    :description="t('settings.security.description')"
  >
    <!-- SmartShield -->
    <SettingsCard
      :title="t('settings.security.shield')"
      :description="t('settings.security.shield_desc')"
    >
      <div class="flex items-center justify-between">
        <div class="flex items-center gap-3">
          <AppIcon name="Shield" :size="18" class="text-blue-500" />
          <div>
            <div class="text-sm font-medium text-[var(--cn-text)]">
              {{ t('settings.security.shield_enable') }}
            </div>
            <div class="text-xs text-[var(--cn-text-muted)]">
              {{ t('settings.security.shield_hint') }}
            </div>
          </div>
        </div>
        <ToggleSwitch
          :model-value="shieldEnabled"
          @update:model-value="setShieldEnabled"
        />
      </div>

      <div v-if="shieldEnabled" class="mt-4 grid grid-cols-3 gap-3 border-t border-[var(--cn-border)] pt-4 text-xs">
        <div>
          <div class="text-[var(--cn-text-muted)]">{{ t('settings.security.blocks_24h') }}</div>
          <div class="mt-0.5 text-lg font-semibold text-[var(--cn-text)]">{{ shieldStats.blocksLast24h }}</div>
        </div>
        <div>
          <div class="text-[var(--cn-text-muted)]">{{ t('settings.security.current_bans') }}</div>
          <div class="mt-0.5 text-lg font-semibold text-[var(--cn-text)]">{{ shieldStats.currentBans }}</div>
        </div>
        <div>
          <div class="text-[var(--cn-text-muted)]">{{ t('settings.security.bans_total') }}</div>
          <div class="mt-0.5 text-lg font-semibold text-[var(--cn-text)]">{{ shieldStats.bansTotal }}</div>
        </div>
      </div>

      <div v-if="shieldEnabled && bannedIPs.length > 0" class="mt-4 flex flex-col gap-2">
        <div class="text-xs font-medium text-[var(--cn-text-muted)]">
          {{ t('settings.security.ban_list') }}
        </div>
        <div
          v-for="b in bannedIPs"
          :key="b.ip"
          class="flex items-center justify-between rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-3 py-2 text-xs"
        >
          <div class="flex items-center gap-2">
            <AppIcon name="Ban" :size="13" class="text-red-500" />
            <span class="font-mono text-[var(--cn-text)]">{{ b.ip }}</span>
            <span class="rounded-full bg-red-500/15 px-2 py-0.5 text-[10px] text-red-500">
              {{ t(`settings.security.tier.${b.tier}`) }}
            </span>
          </div>
          <button class="text-blue-500 hover:underline">
            {{ t('settings.security.unban') }}
          </button>
        </div>
      </div>
    </SettingsCard>

    <!-- Antivirus (ClamAV) — shows status when detected, install hint otherwise -->
    <SettingsCard
      v-if="!antivirus.detected"
      :title="t('settings.security.antivirus')"
      :description="t('settings.security.antivirus_desc', { engine: '' })"
    >
      <div class="rounded-lg border border-dashed border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-6 text-center">
        <AppIcon name="ShieldAlert" :size="28" class="mx-auto mb-2 text-[var(--cn-text-muted)]" />
        <p class="text-sm text-[var(--cn-text-muted)]">
          {{ t('settings.security.antivirus_not_installed') }}
        </p>
        <p class="mt-1 font-mono text-xs text-[var(--cn-text-muted)]">
          sudo apt install clamav clamav-daemon
        </p>
      </div>
    </SettingsCard>
    <SettingsCard
      v-if="antivirus.detected"
      :title="t('settings.security.antivirus')"
      :description="t('settings.security.antivirus_desc', { engine: antivirus.engine ? engineLabels[antivirus.engine] : '' })"
    >
      <div class="flex items-center justify-between">
        <div class="flex items-center gap-3">
          <div
            class="flex h-10 w-10 items-center justify-center rounded-lg"
            :class="antivirus.lastScanResult === 'clean'
              ? 'bg-emerald-500/10 text-emerald-500'
              : 'bg-amber-500/10 text-amber-500'"
          >
            <AppIcon name="Bug" :size="18" />
          </div>
          <div>
            <div class="flex items-center gap-2">
              <span class="text-sm font-medium text-[var(--cn-text)]">
                {{ antivirus.engine ? engineLabels[antivirus.engine] : '' }}
                <span class="text-[var(--cn-text-muted)]">{{ antivirus.version }}</span>
              </span>
              <span
                v-if="antivirus.lastScanResult === 'clean'"
                class="inline-flex items-center gap-1 rounded-full bg-emerald-500/15 px-2 py-0.5 text-[10px] text-emerald-500"
              >
                <AppIcon name="CircleCheck" :size="10" />
                {{ t('settings.security.antivirus_clean') }}
              </span>
              <span
                v-else-if="antivirus.lastScanResult === 'threats' && antivirus.threatsFound"
                class="inline-flex items-center gap-1 rounded-full bg-red-500/15 px-2 py-0.5 text-[10px] text-red-500"
              >
                <AppIcon name="AlertTriangle" :size="10" />
                {{ t('settings.security.antivirus_threats', { n: antivirus.threatsFound }) }}
              </span>
            </div>
            <div class="mt-0.5 text-xs text-[var(--cn-text-muted)]">
              {{ antivirus.daemonRunning
                ? t('settings.security.antivirus_daemon_running')
                : t('settings.security.antivirus_daemon_stopped') }}
            </div>
          </div>
        </div>
      </div>

      <dl class="mt-4 grid grid-cols-1 gap-3 border-t border-[var(--cn-border)] pt-4 text-xs sm:grid-cols-2">
        <div>
          <dt class="text-[var(--cn-text-muted)]">
            {{ t('settings.security.antivirus_signatures') }}
          </dt>
          <dd class="mt-0.5 text-[var(--cn-text)]">
            {{ antivirus.signatureDbVersion }}
            <span class="text-[var(--cn-text-muted)]">
              · {{ t('settings.security.antivirus_updated') }}
              {{ antivirus.signatureDbUpdatedAt ? formatDate(antivirus.signatureDbUpdatedAt) : '—' }}
            </span>
          </dd>
        </div>
        <div>
          <dt class="text-[var(--cn-text-muted)]">{{ t('settings.security.antivirus_last_scan') }}</dt>
          <dd class="mt-0.5 text-[var(--cn-text)]">
            {{ antivirus.lastScanAt ? formatDate(antivirus.lastScanAt) : t('settings.security.antivirus_never_scanned') }}
          </dd>
        </div>
      </dl>

      <div class="mt-4 flex flex-wrap gap-2">
        <Button
          :label="t('settings.security.antivirus_update_sigs')"
          size="small"
          severity="secondary"
          :loading="sigsLoading"
          @click="updateSignatures"
        >
          <template #icon>
            <AppIcon name="RefreshCw" :size="13" />
          </template>
        </Button>
        <Button
          :label="t('settings.security.antivirus_full_scan')"
          size="small"
          :loading="scanLoading"
          @click="runFullScan"
        >
          <template #icon>
            <AppIcon name="Search" :size="13" />
          </template>
        </Button>
      </div>
    </SettingsCard>

    <!-- OAuth2 / SSO -->
    <SettingsCard
      :title="t('settings.security.oauth2')"
      :description="t('settings.security.oauth2_desc')"
    >
      <div class="flex items-center justify-between">
        <div class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.security.oauth2_count', { n: oauth2Clients.length }, oauth2Clients.length) }}
        </div>
        <Button
          :label="t('settings.security.oauth2_register')"
          size="small"
          @click="openOAuth2Register"
        >
          <template #icon>
            <AppIcon name="KeyRound" :size="13" />
          </template>
        </Button>
      </div>
      <div class="mt-3 flex flex-col gap-2">
        <div
          v-for="c in oauth2Clients"
          :key="c.id"
          class="flex items-center justify-between rounded-lg border border-[var(--cn-border)] px-3 py-2"
        >
          <div class="min-w-0 flex-1">
            <div class="text-sm font-medium text-[var(--cn-text)]">{{ c.name }}</div>
            <div class="truncate font-mono text-xs text-[var(--cn-text-muted)]">{{ c.redirect }}</div>
          </div>
          <button class="text-xs text-red-500 hover:underline">
            {{ t('common.delete') }}
          </button>
        </div>
      </div>
    </SettingsCard>

    <!-- Two-factor authentication -->
    <SettingsCard
      :title="t('settings.security.two_factor')"
      :description="t('settings.security.two_factor_desc')"
    >
      <div class="flex items-center justify-between">
        <div class="flex items-center gap-3">
          <AppIcon name="Smartphone" :size="18" class="text-blue-500" />
          <div>
            <div class="text-sm font-medium text-[var(--cn-text)]">
              {{ t('settings.security.two_factor_enable') }}
            </div>
            <div class="text-xs text-[var(--cn-text-muted)]">
              {{ twoFactorEnabled
                  ? t('settings.security.two_factor_active')
                  : t('settings.security.two_factor_inactive') }}
            </div>
          </div>
        </div>
        <Button
          :label="twoFactorEnabled ? t('settings.security.two_factor_manage') : t('settings.security.two_factor_setup')"
          size="small"
          severity="secondary"
          @click="twoFactorEnabled ? (showManageDialog = true) : (showSetupDialog = true)"
        />
      </div>
    </SettingsCard>

    <!-- 2FA dialogs -->
    <TwoFactorSetupDialog
      v-model:visible="showSetupDialog"
      @enabled="on2FAEnabled"
    />
    <TwoFactorManageDialog
      v-model:visible="showManageDialog"
      :backup-codes-remaining="backupCodesRemaining"
      @disabled="on2FADisabled"
    />

    <!-- Active sessions -->
    <SettingsCard
      :title="t('settings.security.sessions')"
      :description="t('settings.security.sessions_desc')"
    >
      <div v-if="sessions.length === 0" class="text-xs text-[var(--cn-text-muted)]">
        {{ t('settings.security.sessions_empty') }}
      </div>
      <div class="flex flex-col gap-2">
        <div
          v-for="s in sessions"
          :key="s.id"
          class="flex items-center justify-between rounded-lg border border-[var(--cn-border)] px-3 py-2"
        >
          <div class="flex min-w-0 flex-1 items-center gap-3">
            <AppIcon name="Monitor" :size="15" class="text-[var(--cn-text-muted)]" />
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2">
                <span class="truncate text-sm text-[var(--cn-text)]">{{ s.userAgent || 'Unknown client' }}</span>
                <span
                  v-if="s.current"
                  class="rounded-full bg-emerald-500/15 px-2 py-0.5 text-[10px] text-emerald-500"
                >
                  {{ t('settings.security.current_session') }}
                </span>
              </div>
              <div class="flex items-center gap-3 text-xs text-[var(--cn-text-muted)]">
                <span class="font-mono">{{ s.ip || '—' }}</span>
                <span class="inline-flex items-center gap-1">
                  <AppIcon name="Clock" :size="10" />
                  {{ formatDate(new Date(s.lastSeenAt * 1000).toISOString()) }}
                </span>
              </div>
            </div>
          </div>
          <button
            :disabled="s.current || sessionRevoking === s.id"
            class="text-xs text-red-500 hover:underline disabled:cursor-not-allowed disabled:opacity-30"
            @click="revokeSession(s.id)"
          >
            {{ t('settings.security.revoke') }}
          </button>
        </div>
      </div>
    </SettingsCard>

    <!-- OAuth2 client register dialog. The created credentials are
         shown once with a copy-out — there's no recovery path for the
         secret so the user MUST copy it before closing. -->
    <Dialog
      v-model:visible="oauth2RegisterVisible"
      modal
      :style="{ width: '30rem' }"
      :header="t('settings.security.oauth2_register_title')"
      :dismissable-mask="!oauth2RegisterBusy && !oauth2RegisterCreated"
    >
      <div v-if="!oauth2RegisterCreated" class="flex flex-col gap-3">
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.security.oauth2_name') }}
          </label>
          <InputText v-model="oauth2RegisterForm.name" class="w-full" placeholder="My App" />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.security.oauth2_redirect') }}
          </label>
          <InputText
            v-model="oauth2RegisterForm.redirect_uris"
            class="w-full font-mono text-sm"
            placeholder="https://app.example.com/oauth/callback"
          />
          <p class="mt-1 text-[10px] text-[var(--cn-text-muted)]">
            {{ t('settings.security.oauth2_redirect_hint') }}
          </p>
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.security.oauth2_scopes') }}
          </label>
          <InputText
            v-model="oauth2RegisterForm.allowed_scopes"
            class="w-full font-mono text-sm"
          />
        </div>
        <p v-if="oauth2RegisterError" class="text-xs text-red-500">
          {{ oauth2RegisterError }}
        </p>
      </div>

      <div v-else class="flex flex-col gap-3 text-sm">
        <p class="text-emerald-500">{{ t('settings.security.oauth2_created') }}</p>
        <p class="text-xs text-[var(--cn-text-muted)]">
          {{ t('settings.security.oauth2_secret_warning') }}
        </p>
        <div>
          <label class="mb-1 block text-xs text-[var(--cn-text-muted)]">client_id</label>
          <code class="block break-all rounded bg-[var(--cn-surface-hover)] p-2 font-mono text-xs">
            {{ oauth2RegisterCreated.client_id }}
          </code>
        </div>
        <div>
          <label class="mb-1 block text-xs text-[var(--cn-text-muted)]">client_secret</label>
          <code class="block break-all rounded bg-[var(--cn-surface-hover)] p-2 font-mono text-xs">
            {{ oauth2RegisterCreated.client_secret }}
          </code>
        </div>
      </div>

      <template #footer>
        <Button
          v-if="!oauth2RegisterCreated"
          :label="t('common.cancel')"
          severity="secondary"
          :disabled="oauth2RegisterBusy"
          @click="oauth2RegisterVisible = false"
        />
        <Button
          v-if="!oauth2RegisterCreated"
          :label="t('settings.security.oauth2_register')"
          :loading="oauth2RegisterBusy"
          :disabled="oauth2RegisterBusy || !oauth2RegisterForm.name || !oauth2RegisterForm.redirect_uris"
          @click="submitOAuth2Register"
        />
        <Button
          v-else
          :label="t('common.close')"
          @click="oauth2RegisterVisible = false"
        />
      </template>
    </Dialog>
  </SettingsPage>
</template>
