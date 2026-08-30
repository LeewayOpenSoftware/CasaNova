<script lang="ts">
import { removePref } from '@/composables/syncedPrefs'

// Module-scope icon cache, shared by every AppCard on the page.
//
// The candidate walk in <script setup> probes an EXTERNAL CDN. Without a
// cache each mount re-ran the whole walk and threw the answer away on
// unmount, so a box with 23 apps fired hundreds of cross-origin requests
// every single time the user came back to the dashboard. Remembering
// which candidate won — or that none did — reduces that to at most one
// request per tile, and to zero for apps we already know have no icon.
// The map is mirrored into localStorage so a reload doesn't start over.
//
// The key deliberately does NOT start with `casanova.`. syncedPrefs
// claims that whole prefix as "account preference synced to the backend"
// (isSyncableKey in composables/syncedPrefs.ts), so the previous
// `casanova.dashboard.iconCache` name got bootstrapped into the prefs
// cache: the next change to ANY preference PUT the entire icon-cache blob
// up to the server, and every hydrate pushed one device's CDN probe
// results down over another's. A local record of which CDN URLs answered
// is not account data — it stays per-browser under the `cn.` prefix,
// which syncedPrefs ignores.
const ICON_CACHE_KEY = 'cn.dashboard.iconCache'
// The name the cache used before the rename. Left behind it would keep
// riding along in the synced prefs blob forever, so purge it through
// syncedPrefs (which drops it from the in-memory prefs cache and from
// localStorage, and the resulting flush strips it from the backend copy).
const ICON_CACHE_LEGACY_KEY = 'casanova.dashboard.iconCache'
// Negative results expire. An app whose artwork lands in selfhst/icons
// later, or one probed while the CDN happened to be unreachable, has to
// get another chance instead of being pinned to the letter tile forever.
// Positive results need no TTL: if the URL breaks the <img> errors, the
// walk resumes and overwrites the entry.
const ICON_NEGATIVE_TTL_MS = 24 * 60 * 60 * 1000
// Bound the persisted map so a box that installs and removes apps over
// months doesn't grow the entry without limit. Oldest writes are pruned.
const ICON_CACHE_CAP = 300

type IconCacheEntry = {
  // Candidate that loaded, or null when every candidate 404'd.
  url: string | null
  // Identity of the inputs the candidate list is built from. When an app
  // is renamed or the backend starts resolving a different iconUrl the
  // candidates change, so the stored verdict no longer applies.
  sig: string
  // Epoch ms of the write; only consulted for negative entries.
  at: number
}

function loadIconCache(): [string, IconCacheEntry][] {
  if (typeof window === 'undefined') return []
  try {
    const raw = window.localStorage.getItem(ICON_CACHE_KEY)
    if (!raw) return []
    const parsed = JSON.parse(raw)
    if (!Array.isArray(parsed)) return []
    return parsed.filter(
      (e): e is [string, IconCacheEntry] =>
        Array.isArray(e) &&
        typeof e[0] === 'string' &&
        !!e[1] &&
        typeof e[1].sig === 'string' &&
        typeof e[1].at === 'number' &&
        (e[1].url === null || typeof e[1].url === 'string'),
    )
  } catch {
    return []
  }
}

const iconCache = new Map<string, IconCacheEntry>(loadIconCache())

// Drop the pre-rename copy, once, when this module first loads. Nothing
// is migrated across: the walk rebuilds the map on demand, and the stale
// negative verdicts are better re-probed than carried over.
try {
  if (typeof window !== 'undefined' && window.localStorage.getItem(ICON_CACHE_LEGACY_KEY) !== null) {
    removePref(ICON_CACHE_LEGACY_KEY)
  }
} catch {
  /* sandboxed — ignore */
}

// Persisting is a courtesy — the in-memory map already serves the session,
// localStorage only has to survive a reload. Writing it straight from
// every <img @load> meant a JSON.stringify of the whole map plus a
// synchronous localStorage write per resolved tile, i.e. a burst of ~23
// of them landing in the same frames the grid is painting. Coalesce the
// burst: the first dirty write arms a timer, later ones ride along, and
// one write covers the lot.
const ICON_CACHE_SAVE_DELAY_MS = 500
let iconCacheSaveTimer: number | null = null

function writeIconCacheNow() {
  try {
    let entries = [...iconCache.entries()]
    if (entries.length > ICON_CACHE_CAP) {
      entries.sort((a, b) => a[1].at - b[1].at)
      entries = entries.slice(entries.length - ICON_CACHE_CAP)
      // Trim the in-memory map too so later saves don't re-prune the
      // same overflow on every write.
      iconCache.clear()
      for (const [k, v] of entries) iconCache.set(k, v)
    }
    window.localStorage.setItem(ICON_CACHE_KEY, JSON.stringify(entries))
  } catch {
    /* sandboxed — ignore */
  }
}

function saveIconCache() {
  if (typeof window === 'undefined') return
  // Already armed: the pending write will pick this change up too. Not a
  // trailing debounce on purpose — a steady trickle of resolutions must
  // still reach localStorage instead of pushing the deadline forever.
  if (iconCacheSaveTimer !== null) return
  iconCacheSaveTimer = window.setTimeout(() => {
    iconCacheSaveTimer = null
    writeIconCacheNow()
  }, ICON_CACHE_SAVE_DELAY_MS) as unknown as number
}

if (typeof window !== 'undefined') {
  // Flush anything still queued when the page goes away, so a reload
  // right after the first paint doesn't have to re-probe the CDN.
  window.addEventListener('pagehide', () => {
    if (iconCacheSaveTimer === null) return
    window.clearTimeout(iconCacheSaveTimer)
    iconCacheSaveTimer = null
    writeIconCacheNow()
  })

  // Because the cache lives outside the `casanova.` namespace,
  // clearLocalPrefs() no longer sweeps it on logout — and it must be
  // swept: the keys are app ids, which would tell the next user on this
  // browser what the previous one had installed. Same 'auth-logout' hook
  // the stores use.
  window.addEventListener('auth-logout', () => {
    if (iconCacheSaveTimer !== null) {
      window.clearTimeout(iconCacheSaveTimer)
      iconCacheSaveTimer = null
    }
    iconCache.clear()
    try {
      window.localStorage.removeItem(ICON_CACHE_KEY)
    } catch {
      /* sandboxed — ignore */
    }
  })
}

function readIconCache(id: string, sig: string): IconCacheEntry | undefined {
  const hit = iconCache.get(id)
  if (!hit || hit.sig !== sig) return undefined
  if (hit.url === null && Date.now() - hit.at > ICON_NEGATIVE_TTL_MS) {
    iconCache.delete(id)
    return undefined
  }
  return hit
}

function writeIconCache(id: string, sig: string, url: string | null) {
  const hit = iconCache.get(id)
  if (hit && hit.sig === sig && hit.url === url) return
  iconCache.set(id, { url, sig, at: Date.now() })
  saveIconCache()
}
</script>

<script setup lang="ts">
// AppCard — one tile in the dashboard's app grid.
//
// Footprint: 80×80 icon square + name label underneath, matching the
// CasaOS reference dashboard. The icon is a colored rounded-square tile
// with the app's initial letter as a fallback until the real icon URL
// loads from the app store.
//
// Navigation: the tile resolves to one of two destinations depending on
// which field the fixture set:
//   1. `internalRoute` → vue-router push (stays inside the SPA shell).
//      Used by CasaNova-native tiles like the VM preview stub.
//   2. `url` → window.open in a new tab. Used by regular Docker apps
//      that expose their own web UI.
// If neither is set the click is a no-op (defensive fallback for
// misconfigured tiles).
//
// Status indicator: a tiny colored dot on the top-right of the icon
// tile communicates running / stopped / updating / unhealthy state
// without taking visual real estate. Preview tiles also get a small
// "Preview" ribbon badge so users know the feature isn't ready yet.

import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import Menu from 'primevue/menu'
import type { MenuItem } from 'primevue/menuitem'
import AppIcon from '@/components/ui/AppIcon.vue'
import type { AppFixture } from '@/types/dashboard'
import ContainerTerminalDialog from '@/components/containers/ContainerTerminalDialog.vue'
import { useContainersStore } from '@/stores/containers'
import { useInstallProgressStore } from '@/stores/installProgress'
import { useIconPackStore } from '@/stores/iconPack'

const props = defineProps<{
  app: AppFixture
}>()

// Icon fallback chain.
//
// resolveIcon() on the API side picks ONE best-guess URL from either
// the CasaOS store_info.icon metadata or a lookup table. In practice
// that single URL 404s a lot:
//   • CasaOS-imported apps ship store_info.icon pointing at
//     icon.casaos.io, which is frequently unreachable from a fresh
//     CasaNova box (DNS, CORS, and sometimes just "404 not found").
//   • Even when the lookup table hits, selfhst/icons occasionally
//     renames files or only ships the svg variant.
//   • The image-name heuristic often guesses slightly wrong (e.g.
//     "linuxserver/sonarr" → "sonarr" works, but "ghcr.io/home-
//     assistant/home-assistant" → "home-assistant" vs the actual
//     "home-assistant-community" filename).
//
// Rather than shipping ONE URL and flipping to a letter tile on the
// first 404, keep a *list* of candidate URLs and advance through them
// on each <img @error>. The user's tile shows the letter fallback
// only once every candidate has been tried.
//
// The list is deliberately short — backend URL, then png+svg for the
// kebab slug and png+svg for the compact slug. It used to fan out to
// ~19 URLs (three slug variants per field × png/svg/webp); those extra
// permutations almost never hit, and multiplied by 23 tiles they turned
// a dashboard visit into hundreds of CDN round-trips. The verdict is
// cached (see the module block above), so the rare app the short list
// misses pays its 404s once instead of on every mount.
//
// The compact variant is back after being cut with the rest: selfhst
// names a fair number of files without the separator ("pihole", not
// "pi-hole"), and CasaOS-imported apps — most of this box's library —
// are exactly the ones whose id/name carries a separator the CDN
// doesn't. Dropping it silently turned working tiles into letter tiles,
// and because a "no icon" verdict is cached for 24 h a reload would not
// have brought them back.
const ICON_CDN = 'https://cdn.jsdelivr.net/gh/selfhst/icons/png'
const ICON_CDN_SVG = 'https://cdn.jsdelivr.net/gh/selfhst/icons/svg'

const iconCandidates = computed<string[]>(() => {
  const urls: string[] = []
  const seen = new Set<string>()
  const add = (u: string | undefined) => {
    if (!u) return
    if (seen.has(u)) return
    seen.add(u)
    urls.push(u)
  }

  // 1. The backend-resolved URL is always tried first — it may point
  //    at a CasaOS or custom CDN we don't know how to probe.
  add(props.app.iconUrl)

  // 2. Normalized kebab slugs from the app id + name. These are the
  //    short identifiers that actually match selfhst filenames.
  const slugify = (raw: string | undefined) =>
    (raw ?? '')
      .toLowerCase()
      .trim()
      .replace(/[^a-z0-9]+/g, '-')
      .replace(/^-+|-+$/g, '')
  const slugs = new Set([slugify(props.app.id), slugify(props.app.name)])

  // 3. For each slug, emit png → svg candidates on the selfhst CDN.
  //    Order matters: png first because <img> renders them without any
  //    extra wrangling and they're the most commonly-populated format
  //    in the repo.
  for (const slug of slugs) {
    if (!slug) continue
    add(`${ICON_CDN}/${slug}.png`)
    add(`${ICON_CDN_SVG}/${slug}.svg`)
  }

  // 4. Same again for the separator-less form, tried only after every
  //    kebab candidate has failed. `add` dedupes, so single-word apps
  //    (where compact === kebab) cost nothing extra: the common case
  //    stays at 7 candidates, still far from the old ~19.
  for (const slug of slugs) {
    const compact = slug.replace(/-/g, '')
    if (!compact) continue
    add(`${ICON_CDN}/${compact}.png`)
    add(`${ICON_CDN_SVG}/${compact}.svg`)
  }

  return urls
})

// Cache identity for this tile: the candidate list is derived from the
// app id (the cache key) plus these two fields, so a change in either
// invalidates the stored verdict.
const iconSig = computed(() => `${props.app.iconUrl ?? ''}|${props.app.name}`)

// Index into iconCandidates. Advances on each @error so the browser
// walks the list. `iconExhausted` means every candidate has been tried
// and the letter tile is authoritative; the index never runs off the
// end because onIconError flips that flag instead.
const iconIndex = ref(0)
const iconExhausted = ref(false)
// True while the current src is a cache hit rather than a probe. A
// remembered winner is rendered without waiting for the tile to scroll
// into view, so a reloaded dashboard looks exactly as the user left it
// instead of flashing letter tiles at them.
const iconFromCache = ref(false)

// Off-screen tiles must not probe: expanded folders and long app lists
// push plenty of tiles below the fold, and their candidate walks are
// pure waste until someone scrolls down.
const root = ref<HTMLElement | null>(null)
const visible = ref(false)
let iconObserver: IntersectionObserver | null = null

// The dashboard grid does not scroll the document: it scrolls inside
// AppLayout's <main class="flex-1 overflow-auto">. An observer left on
// the default root (the viewport) reports every tile clipped by that pane
// as non-intersecting, and rootMargin only inflates the ROOT's rect — it
// cannot see past an ancestor's clip — so the pre-load margin below was
// dead weight: the walk started when the tile was already on screen and
// the user watched the letter flip to the icon while scrolling. Rooting
// at the nearest scrollable ancestor makes the margin count again. Same
// reasoning (and the same walk) as scrollRoot in files/thumbnailLoader.ts.
function scrollRoot(el: Element): Element | null {
  let node = el.parentElement
  while (node) {
    const overflowY = getComputedStyle(node).overflowY
    if (overflowY === 'auto' || overflowY === 'scroll') return node
    node = node.parentElement
  }
  return null
}

function startIconWalk() {
  const hit = readIconCache(props.app.id, iconSig.value)
  if (hit && hit.url === null) {
    // Known to have no artwork — go straight to the letter tile
    // without touching the network at all.
    iconIndex.value = 0
    iconExhausted.value = true
    iconFromCache.value = false
    return
  }
  iconExhausted.value = false
  const known = hit?.url ? iconCandidates.value.indexOf(hit.url) : -1
  iconIndex.value = known >= 0 ? known : 0
  iconFromCache.value = known >= 0
}
startIconWalk()

// Active src: either the current candidate or undefined (which hides
// the <img> and reveals the letter fallback).
const iconSrc = computed<string | undefined>(() => {
  if (iconExhausted.value) return undefined
  if (!visible.value && !iconFromCache.value) return undefined
  const list = iconCandidates.value
  if (list.length === 0) return undefined
  return list[Math.min(iconIndex.value, list.length - 1)]
})

function onIconLoad() {
  const src = iconSrc.value
  if (src) writeIconCache(props.app.id, iconSig.value, src)
}

function onIconError() {
  // Anything past the first failure is a probe again, even if the
  // failing URL came out of the cache.
  iconFromCache.value = false
  const list = iconCandidates.value
  if (iconIndex.value + 1 >= list.length) {
    // All candidates 404'd. Fall back to the letter tile and remember
    // it so the next mount skips the walk entirely.
    iconExhausted.value = true
    writeIconCache(props.app.id, iconSig.value, null)
    return
  }
  iconIndex.value += 1
}

// When the tile's backing app changes (drag-reorder, install,
// uninstall), reset the walker so the new app gets its own verdict —
// from the cache when we have one, from a fresh walk otherwise.
watch(() => [props.app.id, iconSig.value] as const, startIconWalk)

onMounted(() => {
  // Non-browser / no-observer environments (unit tests, ancient engines)
  // fall back to the previous behaviour: probe straight away.
  if (typeof IntersectionObserver === 'undefined' || !root.value) {
    visible.value = true
    return
  }
  iconObserver = new IntersectionObserver(
    (entries) => {
      if (!entries.some((e) => e.isIntersecting)) return
      visible.value = true
      iconObserver?.disconnect()
      iconObserver = null
    },
    // Start the walk slightly before the tile reaches the viewport so
    // the icon is there by the time the user has scrolled to it. A null
    // root falls back to the viewport, which is correct for any host
    // that does put the grid in the document scroller.
    { root: scrollRoot(root.value), rootMargin: '200px' },
  )
  iconObserver.observe(root.value)
})

onBeforeUnmount(() => {
  iconObserver?.disconnect()
  iconObserver = null
})

const router = useRouter()
const { t } = useI18n()

const containersStore = useContainersStore()
const installs = useInstallProgressStore()
const iconPack = useIconPackStore()

// Per-tile action menu. Items depend on the app's current status so
// stopped apps show "Start" while running apps show "Stop/Restart".
const containerTerminalVisible = ref(false)
const containerTerminalMode = ref<'terminal' | 'logs'>('terminal')
const actionLoading = ref(false)

const menuRef = ref<InstanceType<typeof Menu> | null>(null)

function openMenu(event: MouseEvent) {
  event.stopPropagation()
  menuRef.value?.toggle(event)
}

const menuItems = computed<MenuItem[]>(() => {
  const isRunning = props.app.status === 'running' || props.app.status === 'unhealthy'
  return [
    {
      label: t('dashboard.app_menu.open'),
      icon: 'cn-open',
      disabled: !props.app.url && !props.app.internalRoute,
      command: () => onClick(),
    },
    { separator: true },
    isRunning
      ? {
          label: t('dashboard.app_menu.stop'),
          icon: 'cn-stop',
          disabled: actionLoading.value,
          command: () => onLifecycle('stop'),
        }
      : {
          label: t('dashboard.app_menu.start'),
          icon: 'cn-start',
          disabled: actionLoading.value,
          command: () => onLifecycle('start'),
        },
    {
      label: t('dashboard.app_menu.restart'),
      icon: 'cn-restart',
      disabled: !isRunning || actionLoading.value,
      command: () => onLifecycle('restart'),
    },
    // Repair — recreates an app whose container was orphaned from its compose
    // project (shows offline / "not found", won't start). Compose-only.
    ...((props.app.source ?? 'compose') === 'compose'
      ? [{
          label: t('dashboard.app_menu.repair'),
          icon: 'cn-repair',
          disabled: actionLoading.value,
          command: () => onRepair(),
        }]
      : []),
    { separator: true },
    {
      label: t('container.terminal'),
      icon: 'cn-terminal',
      command: () => { containerTerminalMode.value = 'terminal'; containerTerminalVisible.value = true },
    },
    {
      label: t('dashboard.app_menu.logs'),
      icon: 'cn-logs',
      command: () => { containerTerminalMode.value = 'logs'; containerTerminalVisible.value = true },
    },
    {
      label: t('dashboard.app_menu.settings'),
      icon: 'cn-settings',
      command: () => router.push(`/app/apps/${props.app.id}`),
    },
    // Uninstall is compose-only: there is no remove endpoint for unmanaged
    // standalone/external containers, so it's hidden rather than shown as a
    // button that would fail.
    ...((props.app.source ?? 'compose') === 'compose'
      ? [
          { separator: true },
          {
            label: t('apps.uninstall'),
            icon: 'cn-uninstall',
            class: 'text-red-500',
            disabled: actionLoading.value,
            command: () => onUninstall(),
          },
        ]
      : []),
  ]
})

// Status dot color mapping — intentional reuse of the same palette the
// notification levels use so the visual language stays consistent.
const statusDotClass = computed(() => {
  // The LED under the tile, not a badge on top of it. A corner dot competes
  // with the artwork it sits on — worse on a translucent tile, where it also
  // has to fight whatever the wallpaper puts behind it. A thin bar in the
  // gutter reads as an indicator light under the icon: peripheral when
  // everything is running, obvious the moment one goes red.
  switch (props.app.status) {
    case 'running':
      return 'cn-app-led--running'
    case 'stopped':
      return 'cn-app-led--stopped'
    case 'updating':
      return 'cn-app-led--updating'
    case 'unhealthy':
      return 'cn-app-led--unhealthy'
    default:
      return 'cn-app-led--stopped'
  }
})

async function onLifecycle(action: 'start' | 'stop' | 'restart') {
  if (actionLoading.value) return
  actionLoading.value = true
  try {
    const { id, name } = props.app
    let ok = false
    // Route through the source-aware dispatch so standalone/external apps
    // hit the container endpoints instead of the compose ones.
    if (action === 'start') {
      ok = await containersStore.startAppUnified(id)
      if (ok) installs.pushToast('success', `${name} started`)
      else installs.pushToast('error', `Failed to start ${name}`)
    } else if (action === 'stop') {
      ok = await containersStore.stopAppUnified(id)
      if (ok) installs.pushToast('info', `${name} stopped`)
      else installs.pushToast('error', `Failed to stop ${name}`)
    } else {
      ok = await containersStore.restartAppUnified(id)
      if (ok) installs.pushToast('success', `${name} restarted`)
      else installs.pushToast('error', `Failed to restart ${name}`)
    }
  } finally {
    actionLoading.value = false
  }
}

async function onUninstall() {
  if (actionLoading.value) return
  const { id, name } = props.app
  if (!window.confirm(`Uninstall ${name}? The container will be stopped and removed.`)) return
  actionLoading.value = true
  try {
    const ok = await containersStore.uninstallApp(id)
    if (ok) installs.pushToast('success', `${name} uninstalled`)
    else installs.pushToast('error', `Failed to uninstall ${name}`)
  } finally {
    actionLoading.value = false
  }
}

async function onRepair() {
  if (actionLoading.value) return
  const { id, name } = props.app
  if (!window.confirm(t('apps.repair_confirm', { name }))) return
  actionLoading.value = true
  try {
    const ok = await containersStore.repairApp(id)
    if (ok) installs.pushToast('success', t('apps.repair_started', { name }))
    else installs.pushToast('error', t('apps.repair_failed', { name }))
  } finally {
    actionLoading.value = false
  }
}

function onClick() {
  // Internal route takes precedence so preview/native tiles stay inside
  // the SPA. External URLs open in a new tab so the dashboard state is
  // preserved when users jump out to a real app.
  if (props.app.internalRoute) {
    router.push(props.app.internalRoute)
    return
  }
  if (props.app.url) {
    window.open(props.app.url, '_blank', 'noopener,noreferrer')
  }
}
</script>

<template>
  <div ref="root" class="group relative flex flex-col items-center">
    <!-- Quick-actions kebab — top-left, opposite the status dot. Only
         visible on hover or focus so the tile stays calm at rest. -->
    <button
      type="button"
      class="absolute left-0 top-0 z-10 flex h-6 w-6 items-center justify-center rounded-full bg-[var(--cn-surface)] text-[var(--cn-text-muted)] opacity-0 shadow-sm transition-opacity hover:text-[var(--cn-text)] group-hover:opacity-100 group-focus-within:opacity-100"
      :aria-label="t('dashboard.app_menu.more')"
      @click="openMenu"
    >
      <AppIcon name="MoreVertical" :size="13" />
    </button>
    <Menu ref="menuRef" :model="menuItems" :popup="true">
      <template #item="{ item, props: iprops }">
        <a
          v-bind="iprops.action"
          :class="[
            'flex cursor-pointer items-center gap-2 px-3 py-2 text-sm text-[var(--cn-text)]',
            item.disabled ? 'cursor-not-allowed opacity-40' : '',
          ]"
        >
          <AppIcon v-if="item.icon === 'cn-open'" name="ExternalLink" :size="14" />
          <AppIcon v-else-if="item.icon === 'cn-start'" name="Play" :size="14" />
          <AppIcon v-else-if="item.icon === 'cn-stop'" name="Square" :size="14" />
          <AppIcon v-else-if="item.icon === 'cn-restart'" name="RotateCw" :size="14" />
          <AppIcon v-else-if="item.icon === 'cn-repair'" name="Wrench" :size="14" />
          <AppIcon v-else-if="item.icon === 'cn-terminal'" name="Terminal" :size="14" />
          <AppIcon v-else-if="item.icon === 'cn-logs'" name="FileText" :size="14" />
          <AppIcon v-else-if="item.icon === 'cn-settings'" name="Settings" :size="14" />
          <AppIcon v-else-if="item.icon === 'cn-uninstall'" name="Trash2" :size="14" />
          <span>{{ item.label }}</span>
        </a>
      </template>
    </Menu>

  <button
    type="button"
    class="flex flex-col items-center focus:outline-none"
    :aria-label="app.name"
    @click="onClick"
  >
    <div class="relative">
      <!-- Icon tile. The background color is app-specific so each tile
           has a distinct identity even without real artwork.
           Polish layers:
             1. Base color (per-app tileColor)
             2. Glass-highlight gradient overlay — Vista-style top
                bright falloff + darker bottom that makes every tile
                feel like a real rounded-square app icon
             3. Drop-shadow on the embedded icon image so the logo
                reads as floating above the tile surface
             4. Hover lift — translate up + scale + stronger shadow -->
      <div
        :class="[
          'flex h-20 w-20 items-center justify-center rounded-2xl text-3xl font-semibold transition-all duration-200 ease-out group-hover:-translate-y-1 group-hover:scale-[1.03] group-hover:shadow-[0_12px_30px_rgba(0,0,0,0.35)]',
          iconPack.appIconStyle === 'transparent'
            ? 'cn-app-tile--flat text-[var(--cn-text-muted)]'
            : 'cn-app-tile text-white shadow-md',
        ]"
        :style="iconPack.appIconStyle === 'transparent' ? undefined : { backgroundColor: app.tileColor }"
      >
        <!-- Real icon URL takes precedence when present; the initial
             letter is the fallback for fixture / loading states. The
             drop-shadow filter on the img gives the logo a subtle lift
             off the tile surface so it doesn't read as flat. -->
        <img
          v-if="iconSrc"
          :src="iconSrc"
          :alt="app.name"
          :class="[
            'h-12 w-12 object-contain',
            iconPack.appIconStyle === 'transparent' ? '' : '[filter:drop-shadow(0_4px_6px_rgba(0,0,0,0.4))]',
          ]"
          @load="onIconLoad"
          @error="onIconError"
        />
        <span
          v-if="!iconSrc"
          :class="iconPack.appIconStyle === 'transparent' ? '' : '[filter:drop-shadow(0_2px_4px_rgba(0,0,0,0.35))]'"
        >{{ app.initial }}</span>
      </div>
      <!-- Status LED — a slim vertical bar down the tile's left edge.
           Suppressed on preview stubs: those have no container whose state
           there would be to report. -->
      <span
        v-if="!app.preview"
        :class="['cn-app-led', statusDotClass]"
        :aria-label="app.status"
      />
      <!-- Loading overlay while a lifecycle action is in flight -->
      <div
        v-if="actionLoading"
        class="absolute inset-0 flex items-center justify-center rounded-2xl bg-black/40"
      >
        <AppIcon name="RotateCw" :size="20" class="animate-spin text-white" />
      </div>
      <!-- Preview ribbon — bottom-center badge, only for stub tiles.
           Kept separate from the status dot so the two signals don't
           visually compete. -->
      <span
        v-if="app.preview"
        class="absolute -bottom-1 left-0 right-0 mx-auto w-fit rounded-full bg-blue-500/90 px-1.5 py-0.5 text-[9px] font-semibold uppercase tracking-wide text-white shadow"
      >
        {{ t('dashboard.preview') }}
      </span>
    </div>
    <span class="mt-2 max-w-[5.5rem] truncate text-xs font-medium text-[var(--cn-text)]">
      {{ app.name }}
    </span>
  </button>

    <!-- Per-container terminal / logs dialog -->
    <ContainerTerminalDialog
      v-model:visible="containerTerminalVisible"
      :container-id="app.id"
      :container-name="app.name"
      :mode="containerTerminalMode"
    />
  </div>
</template>
