<script setup lang="ts">
// Network settings — Stage 5.4 landing.
//
// Surfaces the Gateway config and TLS state. All state lives locally
// in this component for now; the real wiring is a single call each to
// GET/PUT /v1/gateway and GET/PUT /v1/gateway/tls.

import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import InputText from 'primevue/inputtext'
import InputNumber from 'primevue/inputnumber'
import Button from 'primevue/button'
import ToggleSwitch from 'primevue/toggleswitch'
import Select from 'primevue/select'
import Dialog from 'primevue/dialog'
import dayjs from 'dayjs'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import SettingsCard from '@/components/settings/SettingsCard.vue'
import PhysicalPortsCard from '@/components/settings/PhysicalPortsCard.vue'
import { safeCall } from '@/api/safeCall'
import client from '@/api/client'
import { systemApi } from '@/api'

const { t } = useI18n()

// ----- Gateway port (fetched from real backend) -----
const gatewayPort = ref(80)
const portDirty = ref(false)
function onPortChange() {
  portDirty.value = true
}
// Wave 29: snapshot the value we're about to PUT and only clear the
// dirty flag if the user hasn't typed a new value while the request was
// in flight. The previous version cleared portDirty unconditionally on
// success, so a quick edit-then-save-then-edit-again sequence would
// strand the second edit with portDirty=false (Save button greyed out)
// even though the new value had never reached the gateway.
const portSaveInFlight = ref(false)
async function savePort() {
  if (portSaveInFlight.value) return
  const inFlightValue = gatewayPort.value
  portSaveInFlight.value = true
  try {
    const result = await safeCall(
      async () => {
        await client.put('/v1/gateway/port', { data: String(inFlightValue) })
        return true
      },
    )
    if (result.data && gatewayPort.value === inFlightValue) {
      portDirty.value = false
    }
  } finally {
    portSaveInFlight.value = false
  }
}
onMounted(async () => {
  try {
    const result = await safeCall(
      async () => {
        const res = await client.get<{ success: number; data: string }>('/v1/gateway/port')
        return parseInt(res.data.data, 10) || 80
      },
      () => 80,
    )
    gatewayPort.value = result.data as number
  } catch { /* gateway not reachable */ }
})

// ----- TLS / HTTPS -----
// tlsConfig is the canonical state; the three computed refs below
// forward into it so the existing toggle/form UI keeps working while
// the same object is what we PUT to /v1/sys/certs/config.
const tlsConfig = ref({ enabled: false, domain: '', email: '' })
const tlsSavingFlag = ref(false)
const tlsConfigError = ref('')

const tlsEnabled = computed({
  get: () => tlsConfig.value.enabled,
  set: (v: boolean) => {
    tlsConfig.value.enabled = v
    // Toggling persists immediately so the operator never has to remember
    // a Save click for the enable flag alone. Domain/email still gate on
    // the Save button so we don't fire malformed PUTs mid-typing.
    saveTLSConfig()
  },
})
const tlsDomain = computed({
  get: () => tlsConfig.value.domain,
  set: (v: string) => { tlsConfig.value.domain = v },
})
const tlsEmail = computed({
  get: () => tlsConfig.value.email,
  set: (v: string) => { tlsConfig.value.email = v },
})
const tlsMode = computed<'selfsigned' | 'letsencrypt'>(() =>
  tlsDomain.value.endsWith('.local') ? 'selfsigned' : 'letsencrypt',
)
// Will be populated from GET /v1/gateway/tls when backend supports it.
const tlsExpiresAt = ref<string | null>(null)
const tlsLastRenewal = ref<string | null>(null)

function formatDate(iso: string): string {
  return dayjs(iso).format('MMM D, YYYY')
}

// ----- IP whitelist -----
// Global allow-list persisted by the gateway. Empty list means "no
// restriction" — the gateway middleware short-circuits to allow when
// the list is empty so a fresh install isn't accidentally bricked.
const whitelist = ref<string[]>([])
const newCidr = ref('')
const whitelistBusy = ref(false)
const whitelistError = ref('')

async function loadWhitelist() {
  const res = await safeCall(() => systemApi.getGatewayWhitelist())
  if (res.data) whitelist.value = res.data
}
async function addCidr() {
  const v = newCidr.value.trim()
  if (!v || whitelistBusy.value) return
  whitelistBusy.value = true
  whitelistError.value = ''
  const res = await safeCall(() => systemApi.addGatewayWhitelist(v))
  whitelistBusy.value = false
  if (res.error) {
    whitelistError.value = res.error
    return
  }
  if (res.data) whitelist.value = res.data
  newCidr.value = ''
}
async function removeCidr(cidr: string) {
  if (whitelistBusy.value) return
  whitelistBusy.value = true
  whitelistError.value = ''
  const res = await safeCall(() => systemApi.removeGatewayWhitelist(cidr))
  whitelistBusy.value = false
  if (res.error) {
    whitelistError.value = res.error
    return
  }
  if (res.data) whitelist.value = res.data
}

// ----- Physical ports -----
// The port map (Ethernet/USB/Display visualization + NIC detail) now
// lives in the shared PhysicalPortsCard component, which fetches its own
// hardware snapshot. The Server settings page renders the same card.

// ----- Reverse proxy routes (fetched from gateway) -----
const routes = ref<Array<{ path: string; target: string; port: number }>>([])

onMounted(async () => {
  const result = await safeCall(
    async () => {
      const res = await client.get<{ success: number; data: Record<string, string> }>('/v1/gateway/routes')
      const raw = res.data.data || {}
      return Object.entries(raw).map(([path, target]) => {
        const portMatch = target.match(/:(\d+)/)
        return { path, target, port: portMatch ? parseInt(portMatch[1], 10) : 80 }
      }).sort((a, b) => a.path.localeCompare(b.path))
    },
    () => [
      { path: '/', target: 'CasaNova dashboard', port: 80 },
    ],
  )
  routes.value = result.data as Array<{ path: string; target: string; port: number }>
})

// devMode no longer gates Macvlan (graduated in Wave 5) — the composable
// import was removed too since this was its only use.

// ----- TLS/SSL Certificate Management (Let's Encrypt / ACME) -----
const certStatus = ref<{
  certbot_installed: boolean
  configured: boolean
  domain?: string
  email?: string
  method?: string
  auto_renew?: boolean
  expires?: string
  days_remaining?: number
  cert_path?: string
  key_path?: string
  issuing?: boolean
  issue_error?: string
}>({ certbot_installed: false, configured: false })

const certCheck = ref<{ certbot_installed: boolean; port_80_available: boolean }>({
  certbot_installed: false,
  port_80_available: false,
})
const certLoading = ref(true)
const certDomain = ref('')
const certEmail = ref('')
const certMethod = ref('http')
const certRenewing = ref(false)
const certRemoveDialog = ref(false)
const certRemoving = ref(false)

const certMethodOptions = computed(() => [
  { value: 'http', label: t('settings.network.certs_method_http') },
  { value: 'dns', label: t('settings.network.certs_method_dns') },
])

const certDaysClass = computed(() => {
  const d = certStatus.value.days_remaining ?? 0
  if (d > 30) return 'text-emerald-500'
  if (d > 10) return 'text-amber-500'
  return 'text-red-500'
})

async function loadCertStatus() {
  certLoading.value = true
  try {
    const [statusRes, checkRes] = await Promise.all([
      safeCall(async () => {
        const res = await client.get<{ success: number; data: typeof certStatus.value }>('/v1/sys/certs/status')
        return res.data.data
      }),
      safeCall(async () => {
        const res = await client.get<{ success: number; data: typeof certCheck.value }>('/v1/sys/certs/check')
        return res.data.data
      }),
    ])
    if (statusRes.data) certStatus.value = statusRes.data
    if (checkRes.data) certCheck.value = checkRes.data
  } catch { /* endpoint not available */ }
  certLoading.value = false
}

async function issueCert() {
  const result = await safeCall(async () => {
    const res = await client.post<{ success: number; data: { status: string } }>('/v1/sys/certs/issue', {
      domain: certDomain.value,
      email: certEmail.value,
      method: certMethod.value,
    })
    return res.data.data
  })
  if (result.data) {
    // Poll for completion.
    certStatus.value = { ...certStatus.value, issuing: true, issue_error: '' }
    pollCertStatus()
  }
}

let pollTimer: ReturnType<typeof setTimeout> | null = null
function pollCertStatus() {
  if (pollTimer) clearTimeout(pollTimer)
  pollTimer = setTimeout(async () => {
    try {
      await loadCertStatus()
      if (certStatus.value.issuing && !certStatus.value.issue_error) {
        pollCertStatus()
      } else {
        // Stop polling: either done or error occurred
        pollTimer = null
      }
    } catch (err) {
      console.error('Certificate status polling failed:', err)
      pollTimer = null
    }
  }, 3000)
}

// Clean up cert polling timer when navigating away.
onUnmounted(() => {
  if (pollTimer) {
    clearTimeout(pollTimer)
    pollTimer = null
  }
})

async function renewCert() {
  certRenewing.value = true
  await safeCall(async () => {
    await client.post('/v1/sys/certs/renew')
    return true
  })
  await loadCertStatus()
  certRenewing.value = false
}

// Test renewal — hits the same endpoint with ?dry-run=1 so certbot
// talks to ACME staging, doesn't touch disk, and doesn't burn rate
// limit. We show the raw certbot transcript so the operator can
// inspect what would happen.
const testRenewalBusy = ref(false)
const testRenewalDialogVisible = ref(false)
const testRenewalOutput = ref('')
const testRenewalError = ref('')
async function testRenewCert() {
  if (testRenewalBusy.value) return
  testRenewalBusy.value = true
  testRenewalError.value = ''
  testRenewalOutput.value = ''
  testRenewalDialogVisible.value = true
  const res = await safeCall(async () => {
    const r = await client.post<{ success: number; message: string; data?: { output?: string } }>(
      '/v1/sys/certs/renew?dry-run=1',
    )
    return r.data
  })
  testRenewalBusy.value = false
  if (res.error) {
    testRenewalError.value = res.error
    return
  }
  testRenewalOutput.value = res.data?.data?.output || res.data?.message || ''
}

async function removeCert() {
  certRemoving.value = true
  await safeCall(async () => {
    await client.delete('/v1/sys/certs')
    return true
  })
  certRemoveDialog.value = false
  certRemoving.value = false
  certDomain.value = ''
  certEmail.value = ''
  await loadCertStatus()
}

onMounted(() => {
  loadCertStatus()
  loadWhitelist()
  loadTLSConfig()
})

// ----- TLS config persistence (loaders/savers) -----
// Backed by GET/PUT /v1/sys/certs/config. The tlsConfig ref itself is
// declared up top so the early-page computed forwarders can reference
// it without TDZ issues.
async function loadTLSConfig() {
  const res = await safeCall(() => systemApi.getTLSConfig())
  if (res.data) tlsConfig.value = { ...res.data }
}
async function saveTLSConfig() {
  if (tlsSavingFlag.value) return
  tlsSavingFlag.value = true
  tlsConfigError.value = ''
  const res = await safeCall(() => systemApi.setTLSConfig(tlsConfig.value))
  tlsSavingFlag.value = false
  if (res.error) {
    tlsConfigError.value = res.error
    return
  }
}

// ----- Macvlan Network (Developer Mode) -----
const macvlanEnabled = ref(false)
const macvlanConfig = ref({
  networkName: 'casanova-macvlan',
  parentInterface: 'eth0',
  subnet: '192.168.1.0/24',
  gateway: '192.168.1.1',
  ipRangeStart: '192.168.1.200',
  ipRangeEnd: '192.168.1.250',
  auxAddress: '',
})
const macvlanSaving = ref(false)
const macvlanCreated = ref(false)
const macvlanError = ref<string | null>(null)
const macvlanLoading = ref(false)
// The macvlan read is admin-only. Without this the 403 was swallowed and the
// card rendered its hardcoded defaults (192.168.1.0/24, .200-.250) as if they
// were the box's real configuration — a non-admin was shown a network that does
// not exist. Not knowing is fine; being told a plausible fiction is not.
const macvlanForbidden = ref(false)

// Populated from the real host interfaces via the backend
const availableInterfaces = ref<Array<{ value: string; label: string }>>([
  { value: 'eth0', label: 'eth0 — Ethernet' },
])

type AppAssignment = {
  appId: string
  appName: string
  iconUrl: string
  enabled: boolean
  ipAddress: string
}
const appIpAssignments = ref<AppAssignment[]>([])
const appToggleLoading = ref<string | null>(null)

async function loadMacvlanState() {
  macvlanLoading.value = true
  macvlanError.value = null
  try {
    const res = await client.get<{ success: number; data: {
      enabled: boolean
      config: typeof macvlanConfig.value
      created: boolean
      assignments: AppAssignment[]
      interfaces: Array<{ value: string; label: string }>
    } }>('/v1/sys/network/macvlan')
    const d = res.data.data
    if (d) {
      macvlanEnabled.value = d.enabled
      macvlanCreated.value = d.created
      if (d.config) macvlanConfig.value = { ...macvlanConfig.value, ...d.config }
      if (d.assignments) appIpAssignments.value = d.assignments
      if (d.interfaces?.length) availableInterfaces.value = d.interfaces
    }
  } catch (e: unknown) {
    // Tell "you may not see this" apart from "this did not load". Only the
    // second one may fall back to defaults, and even then they are not
    // presented as current state.
    const status = (e as { response?: { status?: number } })?.response?.status
    if (status === 401 || status === 403) {
      macvlanForbidden.value = true
    }
  } finally {
    macvlanLoading.value = false
  }
}

onMounted(() => {
  // Was previously dev-mode-gated; the backend has shipped — let every
  // admin reach it. The "Save" button is what triggers any destructive
  // docker network call, so just loading the state is harmless.
  loadMacvlanState()
})

async function saveMacvlanConfig() {
  macvlanSaving.value = true
  macvlanError.value = null
  try {
    const res = await client.post<{ success: number; message: string; data: { created: boolean } }>(
      '/v1/sys/network/macvlan',
      { enabled: macvlanEnabled.value, config: macvlanConfig.value },
    )
    if (res.data.success === 0) {
      macvlanCreated.value = macvlanEnabled.value
      // Refresh to get real Docker state
      await loadMacvlanState()
    } else {
      macvlanError.value = res.data.message || 'Failed to save'
    }
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    macvlanError.value = msg
  } finally {
    macvlanSaving.value = false
  }
}

async function toggleAppMacvlan(app: AppAssignment) {
  appToggleLoading.value = app.appId
  macvlanError.value = null
  try {
    const res = await client.put<{ success: number; message: string }>(
      '/v1/sys/network/macvlan/app/' + app.appId,
      { enabled: app.enabled, ipAddress: app.ipAddress },
    )
    if (res.data.success !== 0) {
      macvlanError.value = res.data.message || 'Failed to update app'
      // Revert optimistic toggle
      app.enabled = !app.enabled
    }
  } catch (e: unknown) {
    const msg = e instanceof Error ? e.message : String(e)
    macvlanError.value = msg
    app.enabled = !app.enabled
  } finally {
    appToggleLoading.value = null
  }
}

// ----- SMB / Samba File Sharing -----
type SmbShare = {
  name: string
  path: string
  read_only: boolean
  guest_ok: boolean
  comment: string
  valid_users?: string[]
}

const smbInstalled = ref(false)
const smbRunning = ref(false)
const smbWorkgroup = ref('WORKGROUP')
const smbShares = ref<SmbShare[]>([])
const smbLoading = ref(true)
const smbToggling = ref(false)

const smbShowDialog = ref(false)
const smbEditMode = ref(false)
const smbEditOriginalName = ref('')
const smbForm = ref<SmbShare>({
  name: '',
  path: '',
  read_only: false,
  guest_ok: false,
  comment: '',
})
const smbSaving = ref(false)

const smbDeleteDialog = ref(false)
const smbDeleteTarget = ref<SmbShare | null>(null)
const smbDeleting = ref(false)

async function loadSmbStatus() {
  smbLoading.value = true
  try {
    const res = await client.get<{
      success: number
      data: {
        installed: boolean
        running: boolean
        workgroup: string
        shares: SmbShare[]
      }
    }>('/v1/sys/smb/status')
    const d = res.data.data
    if (d) {
      smbInstalled.value = d.installed
      smbRunning.value = d.running
      smbWorkgroup.value = d.workgroup || 'WORKGROUP'
      smbShares.value = d.shares || []
    }
  } catch { /* endpoint not available */ }
  smbLoading.value = false
}

async function toggleSmb() {
  smbToggling.value = true
  const enabling = !smbRunning.value
  try {
    await client.post('/v1/sys/smb/toggle', { enabled: enabling })
    smbRunning.value = enabling
  } catch { /* failed */ }
  smbToggling.value = false
}

function openAddShare() {
  smbEditMode.value = false
  smbEditOriginalName.value = ''
  smbForm.value = { name: '', path: '', read_only: false, guest_ok: false, comment: '' }
  smbShowDialog.value = true
}

function openEditShare(share: SmbShare) {
  smbEditMode.value = true
  smbEditOriginalName.value = share.name
  smbForm.value = { ...share }
  smbShowDialog.value = true
}

async function saveShare() {
  smbSaving.value = true
  try {
    if (smbEditMode.value) {
      await client.put('/v1/sys/smb/shares/' + encodeURIComponent(smbEditOriginalName.value), smbForm.value)
    } else {
      await client.post('/v1/sys/smb/shares', smbForm.value)
    }
    smbShowDialog.value = false
    await loadSmbStatus()
  } catch { /* failed */ }
  smbSaving.value = false
}

function confirmDeleteShare(share: SmbShare) {
  smbDeleteTarget.value = share
  smbDeleteDialog.value = true
}

async function deleteShare() {
  if (!smbDeleteTarget.value) return
  smbDeleting.value = true
  try {
    await client.delete('/v1/sys/smb/shares/' + encodeURIComponent(smbDeleteTarget.value.name))
    smbDeleteDialog.value = false
    smbDeleteTarget.value = null
    await loadSmbStatus()
  } catch { /* failed */ }
  smbDeleting.value = false
}

onMounted(() => {
  loadSmbStatus()
})
</script>

<template>
  <SettingsPage
    icon="Network"
    :title="t('settings.network.title')"
    :description="t('settings.network.description')"
  >
    <!-- Physical ports — hardware visualization inspired by ZimaOS.
         Shows what the user physically sees on the back of the device:
         RJ45 × 2, USB × 4, HDMI + DisplayPort. Live green dots on
         connected ports mirror the actual link state. Factored into a
         shared card so the Server settings page renders the same map. -->
    <PhysicalPortsCard />

    <!-- Gateway port -->
    <SettingsCard
      :title="t('settings.network.gateway_port')"
      :description="t('settings.network.gateway_port_desc')"
    >
      <div class="flex items-end gap-3">
        <div class="w-32">
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.network.port') }}
          </label>
          <InputNumber
            v-model="gatewayPort"
            :min="1"
            :max="65535"
            :useGrouping="false"
            class="w-full"
            @input="onPortChange"
          />
        </div>
        <Button
          :label="t('common.save')"
          :disabled="!portDirty || portSaveInFlight"
          size="small"
          @click="savePort"
        />
      </div>
    </SettingsCard>

    <!-- HTTPS / TLS -->
    <SettingsCard
      :title="t('settings.network.https')"
      :description="t('settings.network.https_desc')"
    >
      <div class="flex items-center justify-between">
        <div class="flex items-center gap-3">
          <AppIcon name="ShieldCheck" :size="18" class="text-blue-500" />
          <div>
            <div class="text-sm font-medium text-[var(--cn-text)]">
              {{ t('settings.network.enable_https') }}
            </div>
            <div class="text-xs text-[var(--cn-text-muted)]">
              {{ t('settings.network.https_hint') }}
            </div>
          </div>
        </div>
        <ToggleSwitch v-model="tlsEnabled" />
      </div>

      <div v-if="tlsEnabled" class="mt-4 flex flex-col gap-3 border-t border-[var(--cn-border)] pt-4">
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.network.domain') }}
          </label>
          <InputText v-model="tlsDomain" class="w-full max-w-md" />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.network.email') }}
          </label>
          <InputText v-model="tlsEmail" class="w-full max-w-md" />
        </div>
        <div class="flex items-center gap-2">
          <Button
            size="small"
            :loading="tlsSavingFlag"
            :disabled="tlsSavingFlag"
            @click="saveTLSConfig"
          >
            {{ t('common.save') }}
          </Button>
          <span v-if="tlsConfigError" class="text-xs text-red-500">{{ tlsConfigError }}</span>
        </div>

        <dl class="mt-2 grid grid-cols-1 gap-2 rounded-lg bg-[var(--cn-surface-hover)] p-3 text-xs sm:grid-cols-3">
          <div>
            <dt class="text-[var(--cn-text-muted)]">{{ t('settings.network.mode') }}</dt>
            <dd class="mt-0.5 font-medium text-[var(--cn-text)]">
              {{ tlsMode === 'letsencrypt' ? t('settings.network.lets_encrypt') : t('settings.network.self_signed') }}
            </dd>
          </div>
          <div>
            <dt class="text-[var(--cn-text-muted)]">{{ t('settings.network.expires') }}</dt>
            <dd class="mt-0.5 font-medium text-[var(--cn-text)]">{{ tlsExpiresAt ? formatDate(tlsExpiresAt) : '—' }}</dd>
          </div>
          <div>
            <dt class="text-[var(--cn-text-muted)]">{{ t('settings.network.last_renewal') }}</dt>
            <dd class="mt-0.5 font-medium text-[var(--cn-text)]">{{ tlsLastRenewal ? formatDate(tlsLastRenewal) : '—' }}</dd>
          </div>
        </dl>

        <Button
          :label="t('settings.network.test_renewal')"
          severity="secondary"
          size="small"
          :loading="testRenewalBusy"
          :disabled="testRenewalBusy"
          @click="testRenewCert"
        >
          <template #icon>
            <AppIcon name="Zap" :size="13" />
          </template>
        </Button>
      </div>
    </SettingsCard>

    <!-- IP whitelist -->
    <SettingsCard
      :title="t('settings.network.ip_whitelist')"
      :description="t('settings.network.ip_whitelist_desc')"
    >
      <div class="flex flex-wrap gap-2">
        <span
          v-for="cidr in whitelist"
          :key="cidr"
          class="inline-flex items-center gap-1.5 rounded-full border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] px-3 py-1 font-mono text-xs text-[var(--cn-text)]"
        >
          {{ cidr }}
          <button
            type="button"
            class="text-[var(--cn-text-muted)] hover:text-red-500"
            :aria-label="t('common.delete')"
            @click="removeCidr(cidr)"
          >
            ×
          </button>
        </span>
      </div>
      <div class="mt-3 flex items-end gap-2">
        <div class="flex-1 max-w-xs">
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('settings.network.add_cidr') }}
          </label>
          <InputText
            v-model="newCidr"
            placeholder="e.g. 192.168.1.0/24"
            class="w-full"
            @keyup.enter="addCidr"
          />
        </div>
        <Button :label="t('common.create')" size="small" @click="addCidr" />
      </div>
    </SettingsCard>

    <!-- TLS/SSL Certificate Management (Let's Encrypt / ACME) -->
    <SettingsCard
      :title="t('settings.network.certs_title')"
      :description="t('settings.network.certs_desc')"
    >
      <!-- certbot not installed -->
      <div v-if="certLoading" class="flex items-center gap-2 text-xs text-[var(--cn-text-muted)]">
        <AppIcon name="RefreshCw" :size="14" class="animate-spin" />
        Loading...
      </div>

      <div v-else-if="!certCheck.certbot_installed" class="flex items-center gap-3 rounded-lg border border-amber-500/30 bg-amber-500/10 px-4 py-3">
        <AppIcon name="AlertTriangle" :size="18" class="shrink-0 text-amber-500" />
        <span class="text-xs text-amber-600 dark:text-amber-400">{{ t('settings.network.certs_not_installed') }}</span>
      </div>

      <!-- No cert configured — show setup form -->
      <div v-else-if="!certStatus.configured && !certStatus.issuing" class="flex flex-col gap-4">
        <div class="flex items-center gap-3">
          <div class="flex h-10 w-10 items-center justify-center rounded-lg bg-blue-500/10 text-blue-500">
            <AppIcon name="Shield" :size="18" />
          </div>
          <div>
            <div class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.network.certs_no_cert') }}</div>
            <div class="text-xs text-[var(--cn-text-muted)]">{{ t('settings.network.certs_desc') }}</div>
          </div>
        </div>

        <!-- Port 80 check -->
        <div class="flex items-center gap-2 text-xs">
          <span class="font-medium text-[var(--cn-text-muted)]">{{ t('settings.network.certs_port_80') }}:</span>
          <span v-if="certCheck.port_80_available" class="flex items-center gap-1 text-emerald-500">
            <AppIcon name="CheckCircle2" :size="12" />
            {{ t('settings.network.certs_port_80_available') }}
          </span>
          <span v-else class="flex items-center gap-1 text-amber-500">
            <AppIcon name="AlertTriangle" :size="12" />
            {{ t('settings.network.certs_port_80_in_use') }}
          </span>
        </div>

        <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <div>
            <label class="mb-1 block text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">
              {{ t('settings.network.certs_domain') }}
            </label>
            <InputText
              v-model="certDomain"
              :placeholder="t('settings.network.certs_domain_placeholder')"
              class="w-full"
            />
          </div>
          <div>
            <label class="mb-1 block text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">
              {{ t('settings.network.certs_email') }}
            </label>
            <InputText
              v-model="certEmail"
              :placeholder="t('settings.network.certs_email_placeholder')"
              class="w-full"
            />
          </div>
          <div>
            <label class="mb-1 block text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">
              {{ t('settings.network.certs_method') }}
            </label>
            <Select
              v-model="certMethod"
              :options="certMethodOptions"
              option-label="label"
              option-value="value"
              class="w-full"
            />
          </div>
        </div>

        <div>
          <Button
            :label="t('settings.network.certs_issue')"
            size="small"
            :disabled="!certDomain || !certEmail"
            @click="issueCert"
          >
            <template #icon>
              <AppIcon name="Lock" :size="13" />
            </template>
          </Button>
        </div>
      </div>

      <!-- Issuing in progress -->
      <div v-else-if="certStatus.issuing" class="flex flex-col items-center gap-3 py-6">
        <AppIcon name="RefreshCw" :size="24" class="animate-spin text-blue-500" />
        <span class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.network.certs_issuing') }}</span>
      </div>

      <!-- Issue error -->
      <div v-else-if="certStatus.issue_error && !certStatus.configured" class="flex flex-col gap-3">
        <div class="flex items-center gap-3 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-3">
          <AppIcon name="AlertTriangle" :size="18" class="shrink-0 text-red-500" />
          <span class="text-xs text-red-500">{{ certStatus.issue_error }}</span>
        </div>
        <Button
          :label="t('settings.network.certs_issue')"
          size="small"
          severity="secondary"
          @click="certStatus = { ...certStatus, issue_error: '' }"
        />
      </div>

      <!-- Active cert -->
      <div v-else-if="certStatus.configured" class="flex flex-col gap-4">
        <div class="flex items-center justify-between">
          <div class="flex items-center gap-3">
            <div class="flex h-10 w-10 items-center justify-center rounded-lg bg-emerald-500/10 text-emerald-500">
              <AppIcon name="CheckCircle2" :size="18" />
            </div>
            <div>
              <div class="flex items-center gap-2">
                <span class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.network.certs_active') }}</span>
                <span class="rounded-full bg-emerald-500/15 px-2 py-0.5 text-[10px] font-medium text-emerald-500">HTTPS</span>
              </div>
              <div class="font-mono text-xs text-[var(--cn-text-muted)]">{{ certStatus.domain }}</div>
            </div>
          </div>
        </div>

        <dl class="grid grid-cols-1 gap-2 rounded-lg bg-[var(--cn-surface-hover)] p-3 text-xs sm:grid-cols-3">
          <div>
            <dt class="text-[var(--cn-text-muted)]">{{ t('settings.network.certs_expires') }}</dt>
            <dd class="mt-0.5 font-medium text-[var(--cn-text)]">
              {{ certStatus.expires ? formatDate(certStatus.expires) : '---' }}
            </dd>
          </div>
          <div>
            <dt class="text-[var(--cn-text-muted)]">{{ t('settings.network.certs_days_remaining', { n: certStatus.days_remaining ?? 0 }) }}</dt>
            <dd class="mt-0.5 font-medium" :class="certDaysClass">
              <div class="flex items-center gap-1">
                <AppIcon name="Clock" :size="12" />
                {{ certStatus.days_remaining ?? 0 }}
              </div>
            </dd>
          </div>
          <div>
            <dt class="text-[var(--cn-text-muted)]">{{ t('settings.network.certs_method') }}</dt>
            <dd class="mt-0.5 font-medium text-[var(--cn-text)]">
              {{ certStatus.method === 'http' ? 'HTTP-01' : 'DNS-01' }}
            </dd>
          </div>
        </dl>

        <div class="flex items-center gap-2">
          <Button
            :label="t('settings.network.certs_renew')"
            size="small"
            severity="secondary"
            :loading="certRenewing"
            @click="renewCert"
          >
            <template #icon>
              <AppIcon name="RefreshCw" :size="13" />
            </template>
          </Button>
          <Button
            :label="t('settings.network.certs_remove')"
            size="small"
            severity="danger"
            outlined
            @click="certRemoveDialog = true"
          >
            <template #icon>
              <AppIcon name="Trash2" :size="13" />
            </template>
          </Button>
        </div>
      </div>

      <!-- Remove confirmation dialog -->
      <Dialog
        v-model:visible="certRemoveDialog"
        :header="t('settings.network.certs_remove')"
        :style="{ width: '400px' }"
        modal
      >
        <p class="text-sm text-[var(--cn-text-muted)]">
          {{ t('settings.network.certs_remove_confirm') }}
        </p>
        <template #footer>
          <Button
            :label="t('common.cancel')"
            size="small"
            severity="secondary"
            @click="certRemoveDialog = false"
          />
          <Button
            :label="t('settings.network.certs_remove')"
            size="small"
            severity="danger"
            :loading="certRemoving"
            @click="removeCert"
          />
        </template>
      </Dialog>

      <!-- Test renewal result. Streams the certbot --dry-run transcript
           so the operator can see exactly what would happen for the
           real renewal: ACME staging challenge, account check, the
           "Congratulations" line, etc. No state changes server-side. -->
      <Dialog
        v-model:visible="testRenewalDialogVisible"
        modal
        :style="{ width: '40rem' }"
        :header="t('settings.network.test_renewal_title')"
        :dismissable-mask="!testRenewalBusy"
      >
        <div v-if="testRenewalBusy" class="flex items-center gap-2 py-4">
          <AppIcon name="Loader2" :size="18" class="animate-spin text-blue-500" />
          <span class="text-sm text-[var(--cn-text-muted)]">
            {{ t('settings.network.test_renewal_running') }}
          </span>
        </div>
        <div
          v-else-if="testRenewalError"
          class="rounded-lg border border-red-500/30 bg-red-500/10 p-3 text-xs text-red-500"
        >
          {{ testRenewalError }}
        </div>
        <pre
          v-else-if="testRenewalOutput"
          class="max-h-96 overflow-auto whitespace-pre-wrap rounded-lg bg-[var(--cn-surface-hover)] p-3 font-mono text-[11px] text-[var(--cn-text)]"
        >{{ testRenewalOutput }}</pre>
        <template #footer>
          <Button
            :label="t('common.close')"
            severity="secondary"
            :disabled="testRenewalBusy"
            @click="testRenewalDialogVisible = false"
          />
        </template>
      </Dialog>
    </SettingsCard>

    <!-- Reverse proxy routes -->
    <SettingsCard
      :title="t('settings.network.routes')"
      :description="t('settings.network.routes_desc')"
    >
      <div class="overflow-hidden rounded-lg border border-[var(--cn-border)]">
        <table class="w-full text-sm">
          <thead class="bg-[var(--cn-surface-hover)] text-xs uppercase tracking-wide text-[var(--cn-text-muted)]">
            <tr>
              <th class="px-4 py-2 text-left font-medium">{{ t('settings.network.col_path') }}</th>
              <th class="px-4 py-2 text-left font-medium">{{ t('settings.network.col_target') }}</th>
              <th class="px-4 py-2 text-right font-medium">{{ t('settings.network.col_port') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-[var(--cn-border)]">
            <tr v-for="r in routes" :key="r.path">
              <td class="px-4 py-2 font-mono text-xs">
                <span class="inline-flex items-center gap-1.5">
                  <AppIcon name="Globe" :size="13" class="text-[var(--cn-text-muted)]" />
                  {{ r.path }}
                </span>
              </td>
              <td class="px-4 py-2 text-[var(--cn-text)]">{{ r.target }}</td>
              <td class="px-4 py-2 text-right font-mono text-xs text-[var(--cn-text-muted)]">
                :{{ r.port }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </SettingsCard>

    <!-- SMB / Samba File Sharing -->
    <SettingsCard
      :title="t('settings.network.smb_title')"
      :description="t('settings.network.smb_desc')"
    >
      <!-- Loading -->
      <div v-if="smbLoading" class="flex items-center gap-2 text-xs text-[var(--cn-text-muted)]">
        <AppIcon name="RefreshCw" :size="14" class="animate-spin" />
        Loading...
      </div>

      <!-- Not installed -->
      <div v-else-if="!smbInstalled" class="flex items-center gap-3 rounded-lg border border-amber-500/30 bg-amber-500/10 px-4 py-3">
        <AppIcon name="AlertTriangle" :size="18" class="shrink-0 text-amber-500" />
        <span class="text-xs text-amber-600 dark:text-amber-400">{{ t('settings.network.smb_not_installed') }}</span>
      </div>

      <!-- Installed -->
      <template v-else>
        <!-- Status header + toggle -->
        <div class="mb-4 flex items-center justify-between">
          <div class="flex items-center gap-3">
            <div
              class="flex h-10 w-10 items-center justify-center rounded-lg"
              :class="smbRunning ? 'bg-emerald-500/10 text-emerald-500' : 'bg-[var(--cn-surface-hover)] text-[var(--cn-text-muted)]'"
            >
              <AppIcon name="Share2" :size="18" />
            </div>
            <div>
              <div class="flex items-center gap-2">
                <span class="text-sm font-medium text-[var(--cn-text)]">Samba</span>
                <span
                  class="rounded-full px-2 py-0.5 text-[10px] font-medium"
                  :class="smbRunning
                    ? 'bg-emerald-500/15 text-emerald-500'
                    : 'bg-[var(--cn-surface-hover)] text-[var(--cn-text-muted)]'"
                >
                  {{ smbRunning ? t('settings.network.smb_running') : t('settings.network.smb_stopped') }}
                </span>
              </div>
              <div class="text-xs text-[var(--cn-text-muted)]">
                {{ t('settings.network.smb_workgroup') }}: {{ smbWorkgroup }}
              </div>
            </div>
          </div>
          <Button
            :label="smbRunning ? t('settings.network.smb_disable') : t('settings.network.smb_enable')"
            :severity="smbRunning ? 'secondary' : 'primary'"
            size="small"
            :loading="smbToggling"
            @click="toggleSmb"
          >
            <template #icon>
              <AppIcon name="Power" :size="13" />
            </template>
          </Button>
        </div>

        <!-- Shares list -->
        <div class="border-t border-[var(--cn-border)] pt-4">
          <div class="mb-3 flex items-center justify-between">
            <div class="text-xs font-medium text-[var(--cn-text-muted)]">
              {{ t('settings.network.smb_shares') }}
            </div>
            <Button
              :label="t('settings.network.smb_add_share')"
              size="small"
              severity="secondary"
              @click="openAddShare"
            >
              <template #icon>
                <AppIcon name="Plus" :size="13" />
              </template>
            </Button>
          </div>

          <!-- Empty state -->
          <div
            v-if="smbShares.length === 0"
            class="rounded-lg border border-dashed border-[var(--cn-border)] p-6 text-center text-xs text-[var(--cn-text-muted)]"
          >
            {{ t('settings.network.smb_no_shares') }}
          </div>

          <!-- Shares table -->
          <div v-else class="overflow-hidden rounded-lg border border-[var(--cn-border)]">
            <table class="w-full text-sm">
              <thead class="bg-[var(--cn-surface-hover)] text-xs uppercase tracking-wide text-[var(--cn-text-muted)]">
                <tr>
                  <th class="px-4 py-2 text-left font-medium">{{ t('settings.network.smb_share_name') }}</th>
                  <th class="px-4 py-2 text-left font-medium">{{ t('settings.network.smb_share_path') }}</th>
                  <th class="px-4 py-2 text-center font-medium">{{ t('settings.network.smb_share_readonly') }}</th>
                  <th class="px-4 py-2 text-center font-medium">{{ t('settings.network.smb_share_guest') }}</th>
                  <th class="px-4 py-2 text-right font-medium"></th>
                </tr>
              </thead>
              <tbody class="divide-y divide-[var(--cn-border)]">
                <tr v-for="share in smbShares" :key="share.name">
                  <td class="px-4 py-2 text-xs">
                    <span class="inline-flex items-center gap-1.5">
                      <AppIcon name="FolderOpen" :size="13" class="text-[var(--cn-text-muted)]" />
                      <span class="font-medium text-[var(--cn-text)]">{{ share.name }}</span>
                    </span>
                  </td>
                  <td class="px-4 py-2 font-mono text-xs text-[var(--cn-text-muted)]">{{ share.path }}</td>
                  <td class="px-4 py-2 text-center">
                    <span
                      v-if="share.read_only"
                      class="rounded-full bg-amber-500/15 px-2 py-0.5 text-[10px] font-medium text-amber-500"
                    >R/O</span>
                    <span
                      v-else
                      class="rounded-full bg-emerald-500/15 px-2 py-0.5 text-[10px] font-medium text-emerald-500"
                    >R/W</span>
                  </td>
                  <td class="px-4 py-2 text-center">
                    <span
                      v-if="share.guest_ok"
                      class="rounded-full bg-blue-500/15 px-2 py-0.5 text-[10px] font-medium text-blue-500"
                    >Guest</span>
                    <span
                      v-else
                      class="inline-flex items-center gap-1 text-[var(--cn-text-muted)]"
                    >
                      <AppIcon name="Users" :size="11" />
                    </span>
                  </td>
                  <td class="px-4 py-2 text-right">
                    <div class="flex items-center justify-end gap-1">
                      <button
                        type="button"
                        class="rounded p-1 text-[var(--cn-text-muted)] hover:bg-[var(--cn-surface-hover)] hover:text-[var(--cn-text)]"
                        :aria-label="t('settings.network.smb_edit_share')"
                        @click="openEditShare(share)"
                      >
                        <AppIcon name="Edit" :size="13" />
                      </button>
                      <button
                        type="button"
                        class="rounded p-1 text-[var(--cn-text-muted)] hover:bg-red-500/10 hover:text-red-500"
                        :aria-label="t('settings.network.smb_delete_share')"
                        @click="confirmDeleteShare(share)"
                      >
                        <AppIcon name="Trash2" :size="13" />
                      </button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </template>

      <!-- Add/Edit share dialog -->
      <Dialog
        v-model:visible="smbShowDialog"
        :header="smbEditMode ? t('settings.network.smb_edit_share') : t('settings.network.smb_add_share')"
        :style="{ width: '28rem' }"
        modal
        :dismissable-mask="true"
      >
        <div class="flex flex-col gap-4">
          <div>
            <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
              {{ t('settings.network.smb_share_name') }}
            </label>
            <InputText
              v-model="smbForm.name"
              class="w-full"
              placeholder="Media"
            />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
              {{ t('settings.network.smb_share_path') }}
            </label>
            <InputText
              v-model="smbForm.path"
              class="w-full font-mono text-xs"
              placeholder="/DATA/Media"
            />
          </div>
          <div>
            <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
              {{ t('settings.network.smb_share_comment') }}
            </label>
            <InputText
              v-model="smbForm.comment"
              class="w-full"
              :placeholder="t('settings.network.smb_share_comment')"
            />
          </div>
          <div class="flex items-center justify-between">
            <div>
              <div class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.network.smb_share_readonly') }}</div>
            </div>
            <ToggleSwitch v-model="smbForm.read_only" />
          </div>
          <div class="flex items-center justify-between">
            <div>
              <div class="text-sm font-medium text-[var(--cn-text)]">{{ t('settings.network.smb_share_guest') }}</div>
            </div>
            <ToggleSwitch v-model="smbForm.guest_ok" />
          </div>
        </div>

        <template #footer>
          <Button
            :label="t('common.cancel')"
            severity="secondary"
            size="small"
            @click="smbShowDialog = false"
          />
          <Button
            :label="t('common.save')"
            size="small"
            :loading="smbSaving"
            :disabled="!smbForm.name || !smbForm.path"
            @click="saveShare"
          />
        </template>
      </Dialog>

      <!-- Delete confirmation dialog -->
      <Dialog
        v-model:visible="smbDeleteDialog"
        :header="t('settings.network.smb_delete_share')"
        :style="{ width: '400px' }"
        modal
      >
        <p class="text-sm text-[var(--cn-text-muted)]">
          {{ t('settings.network.smb_delete_confirm', { name: smbDeleteTarget?.name ?? '' }) }}
        </p>
        <template #footer>
          <Button
            :label="t('common.cancel')"
            size="small"
            severity="secondary"
            @click="smbDeleteDialog = false"
          />
          <Button
            :label="t('common.delete')"
            size="small"
            severity="danger"
            :loading="smbDeleting"
            @click="deleteShare"
          />
        </template>
      </Dialog>
    </SettingsCard>

    <!-- Macvlan Network — graduated from dev-mode in Wave 5. Each app
         gets its own LAN IP, no port mapping required. -->
    <SettingsCard
      :title="t('settings.network.macvlan_title')"
      :description="t('settings.network.macvlan_desc')"
    >
        <div class="mb-4 flex items-center justify-between">
          <div class="flex items-center gap-3">
            <div class="flex h-10 w-10 items-center justify-center rounded-lg bg-purple-500/10 text-purple-500">
              <AppIcon name="Network" :size="18" />
            </div>
            <div>
              <div class="flex items-center gap-2">
                <span class="text-sm font-medium text-[var(--cn-text)]">Macvlan Mode</span>
                <span class="rounded-full bg-purple-500/15 px-2 py-0.5 text-[10px] font-medium text-purple-500">DEV</span>
              </div>
              <div class="text-xs text-[var(--cn-text-muted)]">
                Each enabled app gets a unique IP on your LAN
              </div>
            </div>
          </div>
          <ToggleSwitch v-model="macvlanEnabled" />
        </div>

        <Transition name="fade">
          <div v-if="macvlanEnabled" class="flex flex-col gap-4">
            <!-- Network configuration -->
            <div class="rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)] p-4">
              <div class="mb-3 text-xs font-medium text-[var(--cn-text-muted)]">Network Configuration</div>
              <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div>
                  <label class="mb-1 block text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">Parent Interface</label>
                  <Select
                    v-model="macvlanConfig.parentInterface"
                    :options="availableInterfaces"
                    option-label="label"
                    option-value="value"
                    class="w-full"
                  />
                </div>
                <div>
                  <label class="mb-1 block text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">Subnet</label>
                  <InputText v-model="macvlanConfig.subnet" class="w-full" placeholder="192.168.1.0/24" />
                </div>
                <div>
                  <label class="mb-1 block text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">Gateway</label>
                  <InputText v-model="macvlanConfig.gateway" class="w-full" placeholder="192.168.1.1" />
                </div>
                <div>
                  <label class="mb-1 block text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">Network Name</label>
                  <InputText v-model="macvlanConfig.networkName" class="w-full" placeholder="casanova-macvlan" />
                </div>
                <div>
                  <label class="mb-1 block text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">IP Range Start</label>
                  <InputText v-model="macvlanConfig.ipRangeStart" class="w-full" placeholder="192.168.1.200" />
                </div>
                <div>
                  <label class="mb-1 block text-[10px] font-medium uppercase tracking-wider text-[var(--cn-text-muted)]">IP Range End</label>
                  <InputText v-model="macvlanConfig.ipRangeEnd" class="w-full" placeholder="192.168.1.250" />
                </div>
              </div>

              <!-- Error banner -->
              <div
                v-if="macvlanError"
                class="mt-3 rounded-lg border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-500"
              >
                {{ macvlanError }}
              </div>

              <div class="mt-4 flex items-center justify-between">
                <div class="text-xs text-[var(--cn-text-muted)]">
                  <span v-if="macvlanForbidden">
                    Macvlan settings are visible to administrators only. The values
                    above are placeholders, not this system's configuration.
                  </span>
                  <span v-else-if="macvlanCreated" class="text-emerald-500">Macvlan network active</span>
                  <span v-else>Network not yet created</span>
                </div>
                <div v-if="!macvlanForbidden" class="flex gap-2">
                  <Button
                    v-if="macvlanCreated"
                    label="Refresh"
                    size="small"
                    severity="secondary"
                    :loading="macvlanLoading"
                    @click="loadMacvlanState"
                  />
                  <Button
                    :label="macvlanCreated ? 'Update Network' : 'Create Network'"
                    size="small"
                    :loading="macvlanSaving"
                    @click="saveMacvlanConfig"
                  />
                </div>
              </div>
            </div>

            <!-- Per-app IP assignment -->
            <div v-if="macvlanCreated">
              <div class="mb-2 text-xs font-medium text-[var(--cn-text-muted)]">Per-App IP Assignment</div>
              <div v-if="appIpAssignments.length === 0" class="rounded-lg border border-dashed border-[var(--cn-border)] p-6 text-center text-xs text-[var(--cn-text-muted)]">
                No apps installed yet. Install apps from the Library to assign IPs.
              </div>
              <div v-else class="flex flex-col gap-2">
                <div
                  v-for="app in appIpAssignments"
                  :key="app.appId"
                  class="flex items-center gap-3 rounded-lg border border-[var(--cn-border)] px-3 py-2 transition-colors"
                  :class="app.enabled ? 'bg-purple-500/5 border-purple-500/30' : ''"
                >
                  <img
                    v-if="app.iconUrl"
                    :src="app.iconUrl"
                    class="h-8 w-8 rounded-lg"
                    :alt="app.appName"
                  />
                  <div v-else class="flex h-8 w-8 items-center justify-center rounded-lg bg-[var(--cn-surface-hover)] text-xs font-bold text-[var(--cn-text-muted)]">
                    {{ app.appName.charAt(0) }}
                  </div>
                  <div class="min-w-0 flex-1">
                    <div class="text-sm font-medium text-[var(--cn-text)]">{{ app.appName }}</div>
                    <div v-if="app.enabled && app.ipAddress" class="font-mono text-xs text-purple-500">
                      {{ app.ipAddress }}
                    </div>
                  </div>
                  <div v-if="app.enabled" class="w-36">
                    <InputText
                      v-model="app.ipAddress"
                      class="w-full"
                      placeholder="192.168.1.20x"
                      @blur="toggleAppMacvlan(app)"
                    />
                  </div>
                  <ToggleSwitch
                    v-model="app.enabled"
                    :disabled="appToggleLoading === app.appId"
                    @update:model-value="toggleAppMacvlan(app)"
                  />
                </div>
              </div>
            </div>

            <!-- Info box -->
            <div class="rounded-lg border border-blue-500/20 bg-blue-500/5 p-3 text-xs text-[var(--cn-text-muted)]">
              <div class="mb-1 font-medium text-blue-500">How macvlan works</div>
              <ul class="ml-4 list-disc space-y-1">
                <li>Each enabled app gets its own IP on your LAN (e.g., 192.168.1.201)</li>
                <li>Apps are directly reachable — no port mapping needed</li>
                <li>Great for Pi-hole (DNS), Home Assistant (mDNS), Plex (DLNA)</li>
                <li>IPs must not overlap with your router's DHCP range</li>
                <li>The CasaNova host cannot directly reach macvlan containers (Docker limitation)</li>
              </ul>
            </div>
          </div>
        </Transition>
      </SettingsCard>
  </SettingsPage>
</template>
