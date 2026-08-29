<script setup lang="ts">
// Apps section — an internal browser for the box's own apps.
//
// This view is deliberately an island. It does not touch the dashboard, the
// tiles, or the apps themselves: it reads the same installed-apps store the
// dashboard reads, asks the server which of those apps consent to being
// framed (see api/embed.ts for why the server must answer), and presents its
// own grid. Chosen apps open inside the panel as tabs; every iframe is
// cross-origin, so an embedded app can neither read the dashboard's
// localStorage (where the session token lives) nor call /v1/* as the user.
// That isolation is the design, not a limitation.
//
// Open iframes stay mounted behind v-show when switching tabs — an app
// reloaded on every switch would lose form state, playback position, and
// terminal scrollback, which defeats the point of keeping it open.

import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useContainersStore } from '@/stores/containers'
import { appHost, resolveBoxLanIp } from '@/composables/useAppOrder'
import {
  probeEmbed,
  openMode,
  fetchRemoteHosts,
  type EmbedProbeResult,
  type OpenMode,
  type RemoteHost,
} from '@/api/embed'
import type { AppEntry } from '@/types/containers'

const { t } = useI18n()
const store = useContainersStore()

// ── candidates ──────────────────────────────────────────────────────
// An app qualifies for the section by having a web UI at all. Everything
// else (databases, agents) simply is not listed — absence is the honest
// rendering for "this has no page to show".

interface Candidate {
  app: AppEntry
  port: number
  scheme: 'http' | 'https'
  url: string
  open: OpenMode
}

const probes = ref<Map<number, EmbedProbeResult>>(new Map())
// Public HTTPS hostnames the box publishes, keyed by the port their rule
// finally lands on. Only rules that target THIS box enter the map: matching
// an app to a rule that forwards elsewhere (the stale crafty → .109 rule on
// this very box) would embed a hostname that answers nothing.
const remoteByPort = ref<Map<number, RemoteHost>>(new Map())
const probing = ref(false)
const probeError = ref('')
const filter = ref('')

const pageIsSecure = window.location.protocol === 'https:'

function appPort(app: AppEntry): number | null {
  if (app.webUIPort) return app.webUIPort
  const p = app.services?.[0]?.ports?.[0]?.host
  return p || null
}

const candidates = computed<Candidate[]>(() => {
  const host = appHost()
  return store.unifiedApps
    .map((app) => {
      const port = appPort(app)
      if (!port) return null
      const scheme = app.webUIProtocol || 'http'
      const path = app.webUIPath && app.webUIPath !== '/' ? app.webUIPath : '/'
      const remote = remoteByPort.value.get(port)
      // Origin choice: on the LAN, the direct URL (faster, no tunnel round
      // trip). Through the tunnel, the published hostname — the only origin
      // a remote visitor can actually reach.
      const url = pageIsSecure && remote
        ? `https://${remote.hostname}${path}`
        : `${scheme}://${host}:${port}${path}`
      return {
        app,
        port,
        scheme,
        url,
        open: openMode(probes.value.get(port), scheme, pageIsSecure, !!remote),
      } as Candidate
    })
    .filter((c): c is Candidate => c !== null)
    .filter((c) => {
      const q = filter.value.trim().toLowerCase()
      return !q || (c.app.title || c.app.id).toLowerCase().includes(q)
    })
    .sort((a, b) => (a.app.title || a.app.id).localeCompare(b.app.title || b.app.id))
})

const embeddable = computed(() => candidates.value.filter((c) => c.open.mode === 'embed'))
const external = computed(() => candidates.value.filter((c) => c.open.mode === 'newtab'))
const lanOnly = computed(() => candidates.value.filter((c) => c.open.mode === 'lan-only'))

// ── probing ─────────────────────────────────────────────────────────

async function loadRemoteHosts() {
  try {
    const hosts = await fetchRemoteHosts()
    remoteByPort.value = new Map(
      hosts.filter((h) => h.onThisBox).map((h) => [h.targetPort, h]),
    )
  } catch {
    // No remote-host data degrades to yesterday's behaviour (remote entry
    // shows LAN-only groups); it must not block the section.
  }
}

async function runProbe() {
  probing.value = true
  probeError.value = ''
  try {
    const targets = [
      ...new Map(
        store.unifiedApps
          .map((a) => {
            const port = appPort(a)
            return port ? ([port, { port, scheme: a.webUIProtocol || 'http' }] as const) : null
          })
          .filter((x): x is readonly [number, { port: number; scheme: 'http' | 'https' }] => x !== null),
      ).values(),
    ]
    if (!targets.length) return
    const results = await probeEmbed(targets)
    probes.value = new Map(results.map((r) => [r.port, r]))
  } catch (e) {
    probeError.value = (e as Error)?.message ?? String(e)
  } finally {
    probing.value = false
  }
}

// ── tabs ────────────────────────────────────────────────────────────

interface OpenTab {
  id: string
  title: string
  iconUrl?: string
  src: string
}

const tabs = reactive<OpenTab[]>([])
const activeTab = ref<string>('') // '' = the grid ("home")

function openApp(c: Candidate) {
  if (c.open.mode === 'newtab') {
    window.open(c.url, '_blank', 'noopener,noreferrer')
    return
  }
  if (c.open.mode === 'lan-only') {
    // The frame is what the browser refuses (mixed content); a top-level
    // navigation to the LAN address is allowed, and for anyone actually on
    // the LAN it simply works. For a genuinely remote visitor the tab dies
    // at the network layer — which the hint under the tile already says.
    window.open(c.url, '_blank', 'noopener,noreferrer')
    return
  }
  const existing = tabs.find((tb) => tb.id === c.app.id)
  if (!existing) {
    tabs.push({ id: c.app.id, title: c.app.title || c.app.id, iconUrl: c.app.iconUrl, src: c.url })
  }
  activeTab.value = c.app.id
}

function closeTab(id: string) {
  const i = tabs.findIndex((tb) => tb.id === id)
  if (i >= 0) tabs.splice(i, 1)
  if (activeTab.value === id) activeTab.value = tabs[Math.min(i, tabs.length - 1)]?.id ?? ''
}

function reloadTab(id: string) {
  // Cross-origin iframes cannot be reloaded via contentWindow; resetting
  // src is the one lever the parent has.
  const tab = tabs.find((tb) => tb.id === id)
  if (!tab) return
  const src = tab.src
  tab.src = 'about:blank'
  requestAnimationFrame(() => {
    tab.src = src
  })
}

onMounted(async () => {
  resolveBoxLanIp()
  loadRemoteHosts()
  if (!store.unifiedApps.length) await store.refresh()
  runProbe()
})
</script>

<template>
  <div class="flex h-full min-h-0 flex-col">
    <!-- Tab bar: home + one tab per open app. -->
    <div class="flex items-center gap-1 border-b border-[var(--cn-border)] px-2 py-1">
      <button
        class="flex items-center gap-1.5 rounded-md px-2.5 py-1.5 text-sm transition-colors"
        :class="activeTab === '' ? 'bg-[var(--cn-surface-hover)] text-[var(--cn-text)]' : 'text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]'"
        @click="activeTab = ''"
      >
        <AppIcon name="LayoutGrid" :size="15" />
        <span>{{ t('apps.home') }}</span>
      </button>

      <div class="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto">
        <div
          v-for="tab in tabs"
          :key="tab.id"
          class="group flex max-w-[200px] shrink-0 cursor-pointer items-center gap-1.5 rounded-md px-2.5 py-1.5 text-sm transition-colors"
          :class="activeTab === tab.id ? 'bg-[var(--cn-surface-hover)] text-[var(--cn-text)]' : 'text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]'"
          @click="activeTab = tab.id"
        >
          <img v-if="tab.iconUrl" :src="tab.iconUrl" class="h-4 w-4 rounded-sm" alt="" />
          <span class="truncate">{{ tab.title }}</span>
          <button
            class="rounded p-0.5 opacity-0 transition-opacity hover:bg-[var(--cn-surface)] group-hover:opacity-100"
            :aria-label="t('apps.close_tab')"
            @click.stop="closeTab(tab.id)"
          >
            <AppIcon name="X" :size="12" />
          </button>
        </div>
      </div>

      <Button
        v-if="activeTab !== ''"
        text
        rounded
        severity="secondary"
        :aria-label="t('apps.reload')"
        :title="t('apps.reload')"
        @click="reloadTab(activeTab)"
      >
        <AppIcon name="RefreshCw" :size="15" />
      </Button>
    </div>

    <!-- Panels. Iframes stay mounted so switching tabs does not reload apps. -->
    <div class="relative min-h-0 flex-1">
      <iframe
        v-for="tab in tabs"
        v-show="activeTab === tab.id"
        :key="tab.id"
        :src="tab.src"
        :title="tab.title"
        class="absolute inset-0 h-full w-full border-0 bg-white"
        allow="fullscreen; clipboard-write"
        sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-modals allow-downloads"
        referrerpolicy="no-referrer"
      />

      <!-- Home grid -->
      <div v-show="activeTab === ''" class="absolute inset-0 overflow-y-auto p-4">
        <div class="mx-auto flex max-w-5xl flex-col gap-6">
          <div class="flex flex-wrap items-center justify-between gap-3">
            <div>
              <h1 class="text-xl font-semibold text-[var(--cn-text)]">{{ t('apps.title') }}</h1>
              <p class="text-xs text-[var(--cn-text-muted)]">{{ t('apps.subtitle') }}</p>
            </div>
            <div class="flex items-center gap-2">
              <InputText v-model="filter" :placeholder="t('apps.filter')" class="w-48" />
              <Button
                text
                rounded
                severity="secondary"
                :aria-label="t('apps.reprobe')"
                :title="t('apps.reprobe')"
                :loading="probing"
                @click="runProbe"
              >
                <AppIcon name="RefreshCw" :size="16" />
              </Button>
            </div>
          </div>

          <p v-if="probeError" class="text-sm text-[var(--cn-danger,#ef4444)]">
            {{ t('apps.probe_failed') }}: {{ probeError }}
          </p>

          <!-- Embeddable apps -->
          <section v-if="embeddable.length">
            <div class="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5">
              <button
                v-for="c in embeddable"
                :key="c.app.id"
                class="cn-widget flex flex-col items-center gap-2 p-4 transition-transform hover:scale-[1.02]"
                @click="openApp(c)"
              >
                <img v-if="c.app.iconUrl" :src="c.app.iconUrl" class="h-12 w-12 rounded-xl" alt="" />
                <div v-else class="flex h-12 w-12 items-center justify-center rounded-xl bg-[var(--cn-surface-hover)] text-lg font-semibold text-[var(--cn-text-muted)]">
                  {{ (c.app.title || c.app.id).charAt(0).toUpperCase() }}
                </div>
                <span class="max-w-full truncate text-sm text-[var(--cn-text)]">{{ c.app.title || c.app.id }}</span>
              </button>
            </div>
          </section>

          <p v-else-if="!probing" class="text-sm text-[var(--cn-text-muted)]">
            {{ t('apps.none_embeddable') }}
          </p>

          <!-- Apps that refuse framing: honest, dimmed, one click to a new tab. -->
          <section v-if="external.length">
            <h2 class="mb-2 text-sm font-medium text-[var(--cn-text-muted)]">{{ t('apps.external_title') }}</h2>
            <p class="mb-3 text-xs text-[var(--cn-text-muted)]">{{ t('apps.external_hint') }}</p>
            <div class="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5">
              <button
                v-for="c in external"
                :key="c.app.id"
                class="cn-widget flex flex-col items-center gap-2 p-4 opacity-60 transition-opacity hover:opacity-90"
                :title="c.open.mode === 'newtab' ? c.open.reason : ''"
                @click="openApp(c)"
              >
                <img v-if="c.app.iconUrl" :src="c.app.iconUrl" class="h-12 w-12 rounded-xl" alt="" />
                <div v-else class="flex h-12 w-12 items-center justify-center rounded-xl bg-[var(--cn-surface-hover)] text-lg font-semibold text-[var(--cn-text-muted)]">
                  {{ (c.app.title || c.app.id).charAt(0).toUpperCase() }}
                </div>
                <span class="max-w-full truncate text-sm text-[var(--cn-text)]">{{ c.app.title || c.app.id }}</span>
                <span class="flex items-center gap-1 text-[10px] text-[var(--cn-text-muted)]">
                  <AppIcon name="ExternalLink" :size="10" />
                  {{ t('apps.opens_new_tab') }}
                </span>
              </button>
            </div>
          </section>

          <!-- Reachable only from the LAN (https page + http app = mixed content). -->
          <section v-if="lanOnly.length">
            <h2 class="mb-2 text-sm font-medium text-[var(--cn-text-muted)]">{{ t('apps.lan_only_title') }}</h2>
            <p class="mb-3 text-xs text-[var(--cn-text-muted)]">{{ t('apps.lan_only_hint') }}</p>
            <div class="grid grid-cols-2 gap-3 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5">
              <button
                v-for="c in lanOnly"
                :key="c.app.id"
                class="cn-widget flex flex-col items-center gap-2 p-4 opacity-60 transition-opacity hover:opacity-90"
                @click="openApp(c)"
              >
                <img v-if="c.app.iconUrl" :src="c.app.iconUrl" class="h-12 w-12 rounded-xl" alt="" />
                <div v-else class="flex h-12 w-12 items-center justify-center rounded-xl bg-[var(--cn-surface-hover)] text-lg font-semibold text-[var(--cn-text-muted)]">
                  {{ (c.app.title || c.app.id).charAt(0).toUpperCase() }}
                </div>
                <span class="max-w-full truncate text-sm text-[var(--cn-text)]">{{ c.app.title || c.app.id }}</span>
                <span class="flex items-center gap-1 text-[10px] text-[var(--cn-text-muted)]">
                  <AppIcon name="ExternalLink" :size="10" />
                  {{ t('apps.lan_direct') }}
                </span>
              </button>
            </div>
          </section>
        </div>
      </div>
    </div>
  </div>
</template>
