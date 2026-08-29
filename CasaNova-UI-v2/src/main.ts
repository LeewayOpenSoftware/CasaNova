// CasaNova UI v2 entrypoint.
//
// Wires up, in order: Pinia (state) → router → i18n → PrimeVue (component
// library with Aura preset) → Tailwind (via imported CSS). The order matters
// because the router guards read from the auth store, which needs Pinia
// registered first, and PrimeVue toasts need the app mounted on the element
// before they can render.

import { createApp, watch } from 'vue'
import { createPinia } from 'pinia'
import PrimeVue, { defaultOptions, type PrimeVueLocaleOptions } from 'primevue/config'
import ToastService from 'primevue/toastservice'
import ConfirmationService from 'primevue/confirmationservice'
import Aura from '@primevue/themes/aura'

// Primary palette override: the Aura preset ships with emerald as default,
// but CasaNova v0.9 keeps the original CasaOS blue identity to minimize
// migration UX shock. PrimeVue's `definePreset` layered on top of Aura lets
// us swap just the primary color ramp without rebuilding the preset.
import { definePreset } from '@primevue/themes'

const CasaNovaPreset = definePreset(Aura, {
  semantic: {
    // blue ramp — matches CasaOS's historical accent so existing users
    // recognize the product visually. All three dark-mode accent uses
    // (buttons, links, focus rings, badges) inherit from this.
    primary: {
      50: '{blue.50}',
      100: '{blue.100}',
      200: '{blue.200}',
      300: '{blue.300}',
      400: '{blue.400}',
      500: '{blue.500}',
      600: '{blue.600}',
      700: '{blue.700}',
      800: '{blue.800}',
      900: '{blue.900}',
      950: '{blue.950}',
    },
  },
})

import App from './App.vue'
import router from './router'
import i18n from './i18n'
import './assets/app.css'

// createI18n's generic overload types `global` as the legacy VueI18n shape;
// in Composition mode it is a Composer whose `locale` is a writable ref.
function i18nLocale(): { value: string } {
  return (i18n.global as unknown as { locale: { value: string } }).locale
}

// PrimeVue's own default config already IS the English table, so snapshot it
// here — at module load, before app.use() installs anything and before any
// locale is applied — instead of hand-writing an English dictionary that would
// drift from the library's. The copy matters: `mergeKeys(defaultOptions, opts)`
// only clones `locale` because we pass one, and the resulting config.locale is
// made reactive and written into by the watcher below. Snapshotting detaches us
// from that object so the English strings can never be overwritten in place.
// A shallow copy is enough: re-seeding only ever replaces whole top-level keys,
// it never reaches into the nested `aria` table or the day/month arrays.
const englishLocale: Partial<PrimeVueLocaleOptions> = { ...defaultOptions.locale }

// Component-internal copy PrimeVue renders itself. Only the strings our
// components can actually surface — no need to mirror the whole table.
//
// en_US is the untouched library snapshot rather than an entry of its own,
// because re-seeding happens through Object.assign: it merges, it does not
// replace, so an entry only takes back the keys it actually carries. en_US used
// to be absent (falling through to `{}`), which assigned nothing and left the
// previous language in the config — switching es_ES → en_US kept "Elegir",
// "Sin resultados", "Sí"/"No" and the password meter in Spanish inside an
// otherwise English UI. The reverse direction never showed it because the
// Spanish dictionary is non-empty.
const primeVueLocales: Record<string, Partial<PrimeVueLocaleOptions>> = {
  en_US: englishLocale,
  es_ES: {
    passwordPrompt: 'Escribe una contraseña',
    weak: 'Débil', medium: 'Media', strong: 'Fuerte',
    emptyMessage: 'Sin resultados', emptySearchMessage: 'Sin resultados',
    emptySelectionMessage: 'Sin selección', emptyFilterMessage: 'Sin resultados',
    choose: 'Elegir', upload: 'Subir', cancel: 'Cancelar',
    accept: 'Sí', reject: 'No', clear: 'Limpiar', apply: 'Aplicar',
  },
  pt_BR: {
    passwordPrompt: 'Digite uma senha',
    weak: 'Fraca', medium: 'Média', strong: 'Forte',
    emptyMessage: 'Sem resultados', emptySearchMessage: 'Sem resultados',
    emptySelectionMessage: 'Nada selecionado', emptyFilterMessage: 'Sem resultados',
    choose: 'Escolher', upload: 'Enviar', cancel: 'Cancelar',
    accept: 'Sim', reject: 'Não', clear: 'Limpar', apply: 'Aplicar',
  },
}

function primeVueLocale(code: string): Partial<PrimeVueLocaleOptions> {
  // A code we have no table for lands on English, matching what vue-i18n
  // renders for it (en_US is its fallbackLocale) — never on "keep whatever the
  // last language left behind".
  return primeVueLocales[code] ?? englishLocale
}

const app = createApp(App)

app.use(createPinia())
app.use(router)
app.use(i18n)
app.use(PrimeVue, {
  theme: {
    preset: CasaNovaPreset,
    options: {
      // Tailwind's `dark` class drives PrimeVue dark mode too, so a single
      // toggle on the root <html> element flips both.
      darkModeSelector: '.dark',
      cssLayer: false,
    },
  },
  // PrimeVue ships its own English strings for component-internal copy
  // (the Password strength meter, empty-list messages, date names…). They
  // are NOT part of our i18n bundle, so the login screen showed "Enter a
  // password" even in Spanish. Seed it from our locale and keep it in sync
  // with the language picker (watcher below).
  locale: primeVueLocale(i18nLocale().value),
})

// Re-seed PrimeVue's own strings whenever the app language changes.
watch(
  () => i18nLocale().value,
  (code) => {
    const cfg = app.config.globalProperties.$primevue
    if (cfg) Object.assign(cfg.config.locale as object, primeVueLocale(code))
  },
)
app.use(ToastService)
app.use(ConfirmationService)

// Global error handlers so unhandled rejections and component errors
// don't vanish silently in production.
app.config.errorHandler = (err, _instance, info) => {
  console.error('[CasaNova] Unhandled component error:', info, err)
}
window.addEventListener('unhandledrejection', (event) => {
  console.error('[CasaNova] Unhandled promise rejection:', event.reason)
})

app.mount('#app')
