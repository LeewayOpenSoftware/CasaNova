// vue-i18n setup. Starts with en_US as the default and es_ES as a second
// locale (matching the user's own language profile). The legacy UI ships
// 31 locales under src/assets/lang/ which we will copy over in bulk later —
// the plan is to keep the same JSON shape so translations don't have to be
// redone, only the keys our new components use.
//
// Only en_US is bundled; es_ES and pt_BR are code-split. All three used to sit
// in the entry chunk, so every session downloaded and parsed ~186 KB of
// messages it would never render. en_US stays eager because every session does
// use it: it is the fallback locale, and by policy new keys land in en_US first,
// so a translated locale resolves the not-yet-translated ones through it.

import { watch } from 'vue'
import { createI18n } from 'vue-i18n'
import en_US from './locales/en_US.json'

// Type-safe message shape (derived from en_US as the source of truth).
export type MessageSchema = typeof en_US

type LazyLocale = 'es_ES' | 'pt_BR'

const lazyLocales: Record<LazyLocale, () => Promise<{ default: MessageSchema }>> = {
  es_ES: () => import('./locales/es_ES.json'),
  pt_BR: () => import('./locales/pt_BR.json'),
}

// Every locale is registered up front, empty until its chunk lands. The empty
// placeholders are load-bearing rather than filler: `availableLocales` is what
// builds the picker in Settings → General, and what applyHydratedLocale() in
// syncedPrefs checks before applying the language the backend holds for this
// account. A locale missing from this object would be one the user cannot pick
// and that hydration would silently refuse to restore.
const messages = {
  en_US,
  es_ES: {} as MessageSchema,
  pt_BR: {} as MessageSchema,
}

// Detect the user's preferred locale once at startup. Order of precedence:
//   1. localStorage override ("casanova.lang") — user explicitly chose
//   2. navigator.language — browser preference
//   3. en_US — fallback
// Note: initialLocale must run synchronously before syncedPrefs is
// imported (createI18n is called at module-load time). We read
// localStorage directly here — syncedPrefs.getPref bootstraps from
// the same localStorage values so the answer is consistent. The
// backend-hydrated value will reapply after login via
// syncedPrefs.applyHydratedLocale().
function initialLocale(): string {
  const stored = localStorage.getItem('casanova.lang')
  if (stored) return stored
  const browser = navigator.language?.replace('-', '_') || 'en_US'
  return browser in messages ? browser : 'en_US'
}

const startupLocale = initialLocale()

const i18n = createI18n<[MessageSchema], 'en_US' | 'es_ES' | 'pt_BR'>({
  legacy: false, // Composition API mode
  locale: startupLocale,
  fallbackLocale: 'en_US',
  messages,
})

// createI18n's generic overload types `global` as the legacy VueI18n shape; in
// Composition mode it is really a Composer, whose `locale` is a writable ref.
// Narrow it explicitly (same trick as main.ts and syncedPrefs).
const composer = i18n.global as unknown as {
  locale: { value: string }
  setLocaleMessage: (locale: string, message: MessageSchema) => void
  getLocaleMessage: (locale: string) => Partial<MessageSchema>
}

const pending: Partial<Record<LazyLocale, Promise<void>>> = {}

// Pause before the single retry below. Short enough that a failed startup
// preload doesn't visibly delay the first paint, long enough to clear the
// window where a deploy is mid-way through rewriting www.
const RETRY_DELAY_MS = 300

// Ceiling on how long the startup preload may hold app.mount() back. The
// preload is a network request like any other, and a request that hangs
// (rather than failing) would otherwise park the whole UI on a blank page for
// as long as the socket stays open. Past this point we mount and let the load
// finish on its own — vue-i18n renders the en_US fallback meanwhile and swaps
// to the real strings the moment setLocaleMessage lands, so a slow link costs
// a late repaint instead of a white screen. Deliberately generous: the locale
// chunks are tens of KB, so only a pathological connection reaches it.
const STARTUP_PRELOAD_TIMEOUT_MS = 8000

// Import a locale chunk, retrying once. The failures we actually see are
// transient — the chunk 404s for the couple of seconds a deploy spends
// rewriting www, the Cloudflare tunnel drops a request, the gateway answers
// one 5xx — and the cost of not retrying is a whole session stuck rendering
// the fallback language. Note the retry is not a guarantee: browsers memoize a
// failed module fetch in the module map, so for some failure modes the second
// import rejects instantly off that cached failure. That is what the
// fallBackIfUnloaded() safety net below is for; the retry is simply the cheap
// half of the fix.
async function importLocale(locale: LazyLocale): Promise<{ default: MessageSchema }> {
  try {
    return await lazyLocales[locale]()
  } catch (err) {
    console.warn('[i18n] locale chunk failed to load; retrying once', locale, err)
    await new Promise((resolve) => window.setTimeout(resolve, RETRY_DELAY_MS))
    return lazyLocales[locale]()
  }
}

// Pull a locale's messages in, once. Everything that switches language just
// writes i18n's locale ref — the picker in Settings → General, and
// applyHydratedLocale() in syncedPrefs when login brings down a language this
// browser had never seen — so the watch below is what turns that write into a
// fetch. Until the chunk resolves vue-i18n renders the en_US fallback, which is
// what the session was already showing at that point anyway.
function loadLocaleMessages(code: string): Promise<void> {
  if (!(code in lazyLocales)) return Promise.resolve()
  const locale = code as LazyLocale
  let inflight = pending[locale]
  if (!inflight) {
    inflight = importLocale(locale).then(
      (mod) => {
        composer.setLocaleMessage(locale, mod.default)
      },
      (err) => {
        // Drop the memo so a later switch retries instead of the language being
        // stuck on the en_US fallback for the rest of the session.
        delete pending[locale]
        console.warn('[i18n] could not load locale; falling back to en_US', locale, err)
      },
    )
    pending[locale] = inflight
  }
  return inflight
}

// Safety net for a load that gave up: step the live locale back to en_US when
// its messages never arrived.
//
// Without this the session is wedged. vue-i18n still reports the locale as,
// say, es_ES — so the picker in Settings → General shows Español selected —
// while every string renders through the en_US fallback. To the user that IS
// "my language reset itself", and it is unfixable from the UI: re-picking
// Español writes the same value the locale ref already holds, Vue sees no
// change, the watch never fires and nothing retries the chunk. Stepping the
// ref back to en_US makes the picker agree with what is on screen and turns
// that re-pick into a real change again, which is what re-triggers the load.
//
// Only the in-memory ref moves. The user's persisted choice in
// localStorage("casanova.lang") is left exactly as it was, so a plain reload
// still tries their language first — and applyHydratedLocale() in syncedPrefs,
// which compares the hydrated value against this ref, now finds a difference
// after login and re-triggers the load for free.
//
// Called only once a load has settled, and only if the locale is still the one
// that was being loaded — never mid-flight (messages are legitimately empty
// while the chunk is in the air) and never over a language the user picked in
// the meantime.
function fallBackIfUnloaded(code: string): void {
  if (code === 'en_US' || composer.locale.value !== code) return
  const loaded = composer.getLocaleMessage(code)
  if (loaded && Object.keys(loaded).length > 0) return
  console.warn('[i18n] locale has no messages after loading; falling back to en_US', code)
  composer.locale.value = 'en_US'
}

watch(
  () => composer.locale.value,
  (code) => {
    void loadLocaleMessages(code).then(() => fallBackIfUnloaded(code))
  },
)

// Preload the locale this session actually renders, before anything mounts.
// Top-level await, so main.ts's `import i18n from './i18n'` — and therefore
// app.mount() — waits for it. Letting the first paint land on the en_US
// fallback instead would look exactly like the "my language keeps resetting"
// bug we just closed.
//
// This is the one load nothing else would ever retry — the watch above only
// fires on a *change* of locale, and at startup there is none — so the failure
// path is chained here too rather than left to log a warning and be forgotten.
const startupPreload = loadLocaleMessages(startupLocale).then(() =>
  fallBackIfUnloaded(startupLocale),
)

await Promise.race([
  startupPreload,
  new Promise<void>((resolve) => window.setTimeout(resolve, STARTUP_PRELOAD_TIMEOUT_MS)),
])

export default i18n
