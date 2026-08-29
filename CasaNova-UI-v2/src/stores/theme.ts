// Theme store — runtime appearance preferences.
//
// Backs the General → Appearance subsection: accent color, opt-in
// transparency, login background, dashboard background, and the
// "replicate background in the empty void" toggle. Every setting is
// persisted to localStorage and applied to <html> / CSS variables so
// the change is instant across the whole app (no rebuild, no reload).
//
// The accent color is written to both CasaNova's own `--cn-accent*`
// variables AND PrimeVue Aura's `--p-primary-*` ramp so PrimeVue
// components (Button, ToggleSwitch, Select, etc.) pick up the change
// without any component-level refactor.

import { defineStore } from 'pinia'
import { ref, watch } from 'vue'
import { getPref, setPref, prefsGeneration } from '@/composables/syncedPrefs'

const STORAGE_KEY = 'casanova.theme'
// The login-screen wallpaper is a per-DEVICE preference kept OUTSIDE the
// syncable casanova.* namespace on purpose: the login screen is only seen
// logged-out, after logout()'s clearLocalPrefs() has wiped every casanova.*
// key. A plain key survives that wipe, so the wallpaper the user picked is
// still there on the login screen (and shared by whoever logs in on this
// browser, which is the right model for a pre-auth screen).
const LOGIN_BG_KEY = 'cn.login_bg'

export type GaugeStyle = 'ring' | 'digital' | 'analog' | 'cockpit'

// Theme variant — sets the visual "personality" overlay. `default` is
// the flat modernized CasaOS look the rest of the app was built
// against. `longhorn` overlays glossy gradients + frosted-glass cards
// inspired by Microsoft's Longhorn Aero concept (4074 build mockups
// specifically). `custom:<id>` slots in a user-uploaded theme pack —
// reserved syntax for v0.1, the upload flow lands later.
export type ThemeVariant = 'default' | 'longhorn' | 'zethus' | 'zaru' | `custom:${string}`

export interface ThemePreferences {
  accentColor: string // hex, e.g. '#3b82f6'
  darkMode: boolean
  transparencyEnabled: boolean
  loginBackgroundUrl: string | null
  dashboardBackgroundUrl: string | null
  replicateBackground: boolean
  // Dashboard gauge style. Lives here (not in useUserPrefs) because
  // it's a purely visual preference, and because Pinia guarantees a
  // single store instance across HMR reloads — which matters for
  // reactivity from components mounted before the composable module
  // was last edited.
  gaugeStyle: GaugeStyle
  // Active theme variant. Defaults to 'default' on first load so the
  // app looks exactly like the pre-themes build until the user picks
  // something else.
  themeVariant: ThemeVariant
}

// CasaOS historical blue. Mirrors the default we hard-coded in main.ts
// for PrimeVue's preset, so if the user never touches Appearance the
// app looks exactly as it did before this store existed.
const DEFAULTS: ThemePreferences = {
  accentColor: '#3b82f6',
  darkMode: false,
  transparencyEnabled: false,
  loginBackgroundUrl: null,
  dashboardBackgroundUrl: null,
  replicateBackground: false,
  gaugeStyle: 'ring',
  themeVariant: 'default',
}

function loadFromStorage(): ThemePreferences {
  if (typeof window === 'undefined') return { ...DEFAULTS }
  try {
    // Wave 29: read through syncedPrefs so the same key participates
    // in per-user backend sync. Behavior is identical until hydrate
    // returns from the backend, at which point the watcher below
    // re-runs against the freshly hydrated value.
    const raw = getPref(STORAGE_KEY)
    if (!raw) return applyDeviceLoginBg({ ...DEFAULTS })

    // Migration: the old GeneralSettings toggle wrote plain strings
    // 'dark' or 'light' under the same key. Detect that and convert
    // to the new JSON format so existing users keep their preference.
    if (raw === 'dark' || raw === 'light') {
      const migrated: ThemePreferences = {
        ...DEFAULTS,
        darkMode: raw === 'dark',
      }
      return applyDeviceLoginBg(migrated)
    }

    const parsed = JSON.parse(raw) as Partial<ThemePreferences>
    return applyDeviceLoginBg({ ...DEFAULTS, ...parsed })
  } catch {
    return applyDeviceLoginBg({ ...DEFAULTS })
  }
}

// Overlay the per-device login wallpaper (LOGIN_BG_KEY) onto whatever the
// synced theme prefs say. Runs in every loadFromStorage path so the login
// screen keeps its wallpaper even after logout wiped the casanova.* keys.
function applyDeviceLoginBg(base: ThemePreferences): ThemePreferences {
  if (typeof window === 'undefined') return base
  try {
    const dev = window.localStorage.getItem(LOGIN_BG_KEY)
    if (dev !== null) base.loginBackgroundUrl = dev || null
  } catch {
    /* ignore */
  }
  return base
}

// Convert a #rrggbb hex string to an { r, g, b } tuple. Used when we
// write the accent color into PrimeVue's `--p-primary-*` ramp — Aura
// expects the ramp stops as literal colors, so we derive a matching
// ramp from the chosen accent by light/dark mixing against white/black.
function hexToRgb(hex: string): { r: number; g: number; b: number } {
  const h = hex.replace('#', '')
  const n = parseInt(h.length === 3 ? h.split('').map((c) => c + c).join('') : h, 16)
  return { r: (n >> 16) & 255, g: (n >> 8) & 255, b: n & 255 }
}

// Blend a color toward white (positive amount) or black (negative).
// Returns the mixed color as a hex string. Used to synthesize the
// 50/100/200/.../900 ramp from a single accent pick.
function mix(hex: string, towardWhite: number): string {
  const { r, g, b } = hexToRgb(hex)
  const t = Math.max(-1, Math.min(1, towardWhite))
  const base = t >= 0 ? 255 : 0
  const amount = Math.abs(t)
  const mixChan = (c: number) => Math.round(c * (1 - amount) + base * amount)
  const [mr, mg, mb] = [mixChan(r), mixChan(g), mixChan(b)]
  return '#' + [mr, mg, mb].map((c) => c.toString(16).padStart(2, '0')).join('')
}

export const useThemeStore = defineStore('theme', () => {
  const prefs = ref<ThemePreferences>(loadFromStorage())

  // Apply the current preferences to the live DOM. Idempotent — safe
  // to call whenever anything changes. Called once on store init and
  // on every watched change below.
  function apply() {
    const root = document.documentElement
    const accent = prefs.value.accentColor

    // CasaNova's own variables. We publish both the hex form and a
    // space-separated RGB triplet so Tailwind can use it with
    // slash-alpha syntax: `rgb(var(--cn-accent-rgb) / <alpha>)`. The
    // Tailwind config maps `blue.500` → this RGB var so every
    // `bg-blue-500`, `text-blue-500/10`, etc. picks up the runtime
    // accent system-wide.
    root.style.setProperty('--cn-accent', accent)
    root.style.setProperty('--cn-accent-hover', mix(accent, -0.15))
    const { r, g, b } = hexToRgb(accent)
    root.style.setProperty('--cn-accent-rgb', `${r} ${g} ${b}`)
    root.style.setProperty('--cn-accent-hover-rgb', (() => {
      const hover = mix(accent, -0.15)
      const { r: hr, g: hg, b: hb } = hexToRgb(hover)
      return `${hr} ${hg} ${hb}`
    })())
    root.style.setProperty('--cn-accent-soft', `rgb(${r} ${g} ${b} / 0.15)`)

    // PrimeVue Aura ramp — writing 50..950 mirrors what Aura expects.
    // The ramp is coarse (straight linear mix) but visually cohesive
    // enough for any sensible accent pick; a perceptually-uniform
    // OKLCH ramp is a later polish.
    const ramp: Record<number, number> = {
      50: 0.95, 100: 0.88, 200: 0.72, 300: 0.52, 400: 0.26,
      500: 0, 600: -0.12, 700: -0.24, 800: -0.38, 900: -0.5, 950: -0.62,
    }
    for (const [stop, t] of Object.entries(ramp)) {
      root.style.setProperty(`--p-primary-${stop}`, mix(accent, t))
    }

    // Background slots.
    root.style.setProperty(
      '--cn-login-bg',
      prefs.value.loginBackgroundUrl ? `url('${prefs.value.loginBackgroundUrl}')` : 'none',
    )
    root.style.setProperty(
      '--cn-dashboard-bg',
      prefs.value.dashboardBackgroundUrl ? `url('${prefs.value.dashboardBackgroundUrl}')` : 'none',
    )

    // Dark mode — the `.dark` class on <html> drives both Tailwind's
    // dark: variants and PrimeVue Aura's dark mode (configured via
    // darkModeSelector in main.ts).
    root.classList.toggle('dark', prefs.value.darkMode)

    // Boolean class toggles — drive CSS selectors in app.css.
    root.classList.toggle('cn-has-login-bg', !!prefs.value.loginBackgroundUrl)
    root.classList.toggle('cn-bg-replicate', prefs.value.replicateBackground && !!prefs.value.dashboardBackgroundUrl)
    root.classList.toggle('cn-transparent', prefs.value.transparencyEnabled)

    // Theme variant — overlay class on <html>. We sweep every
    // cn-theme-* class off first so switching from longhorn → custom
    // → default doesn't leave a stale variant in the class list.
    const cl = root.classList
    for (let i = cl.length - 1; i >= 0; i--) {
      const c = cl.item(i)
      if (c && c.startsWith('cn-theme-')) cl.remove(c)
    }
    const variant = prefs.value.themeVariant
    if (variant && variant !== 'default') {
      // For built-in variants the class is `cn-theme-longhorn`;
      // for uploaded packs (`custom:my-pack-id`) the class is
      // `cn-theme-custom-my-pack-id` so the CSS injected from the
      // pack's tokens.css can target a unique selector.
      const id = variant.startsWith('custom:')
        ? 'custom-' + variant.slice('custom:'.length)
        : variant
      cl.add(`cn-theme-${id}`)
    }

    // Inject the uploaded pack's tokens.css via a <link>. The element
    // is reused (one slot for all packs) so toggling variants doesn't
    // pile up stale stylesheets in the head. Built-in variants
    // (default, longhorn) live in app.css and don't need a link.
    const LINK_ID = 'cn-theme-pack-css'
    let link = document.getElementById(LINK_ID) as HTMLLinkElement | null
    if (variant.startsWith('custom:')) {
      const packId = variant.slice('custom:'.length)
      const href = `/v1/sys/themes/${encodeURIComponent(packId)}/tokens.css`
      if (!link) {
        link = document.createElement('link')
        link.id = LINK_ID
        link.rel = 'stylesheet'
        document.head.appendChild(link)
      }
      if (link.getAttribute('href') !== href) {
        link.setAttribute('href', href)
      }
    } else if (link) {
      link.parentNode?.removeChild(link)
    }
  }

  function persist() {
    try {
      // Wave 29: setPref writes the warm localStorage cache AND
      // schedules a debounced backend flush, so a single watcher
      // tick keeps both layers in sync.
      setPref(STORAGE_KEY, JSON.stringify(prefs.value))
    } catch {
      // Ignore storage errors in sandboxed contexts.
    }
  }

  // Setters — one per preference. Separate methods rather than a
  // generic update() so the Appearance UI stays explicit about what's
  // being changed and so future persistence-layer hooks (e.g. sync to
  // the server) can target individual mutations.
  function setDarkMode(enabled: boolean) {
    prefs.value.darkMode = enabled
  }
  function setAccentColor(hex: string) {
    prefs.value.accentColor = hex
  }
  function setTransparency(enabled: boolean) {
    prefs.value.transparencyEnabled = enabled
  }
  function setLoginBackground(url: string | null) {
    prefs.value.loginBackgroundUrl = url
    // Persist to the per-device key so it survives logout (see LOGIN_BG_KEY).
    try {
      if (url) window.localStorage.setItem(LOGIN_BG_KEY, url)
      else window.localStorage.removeItem(LOGIN_BG_KEY)
    } catch {
      /* ignore */
    }
  }
  function setDashboardBackground(url: string | null) {
    prefs.value.dashboardBackgroundUrl = url
  }
  function setReplicateBackground(enabled: boolean) {
    prefs.value.replicateBackground = enabled
  }
  function setGaugeStyle(style: GaugeStyle) {
    prefs.value.gaugeStyle = style
  }
  function setThemeVariant(variant: ThemeVariant) {
    prefs.value.themeVariant = variant
  }
  function reset() {
    prefs.value = { ...DEFAULTS }
  }

  // Skip the next persist tick when set — used by the hydrate-rehydrate
  // path to avoid echoing freshly downloaded values straight back to
  // the backend.
  let suppressNextPersist = false

  // Single deep watcher — any change rewrites the DOM + localStorage.
  // Cheap because the store's state is tiny (5 scalar fields).
  watch(
    prefs,
    () => {
      apply()
      if (suppressNextPersist) {
        suppressNextPersist = false
        return
      }
      persist()
    },
    { deep: true },
  )

  // Wave 29: when syncedPrefs.hydrateFromBackend() finishes, reload the
  // theme blob from the (now-fresh) cache so the user's saved theme
  // takes effect on a brand-new browser/device. The generation counter
  // bumps once per successful hydrate; we re-read the JSON and apply.
  // suppressNextPersist stops the deep watcher from echoing the value
  // back to the backend on the same tick.
  const gen = prefsGeneration()
  watch(gen, () => {
    suppressNextPersist = true
    prefs.value = loadFromStorage()
  })

  // Initial apply on store creation so preferences from a previous
  // session take effect as soon as the app mounts.
  if (typeof window !== 'undefined') {
    apply()
  }

  return {
    prefs,
    setDarkMode,
    setAccentColor,
    setTransparency,
    setLoginBackground,
    setDashboardBackground,
    setReplicateBackground,
    setGaugeStyle,
    setThemeVariant,
    reset,
  }
})
