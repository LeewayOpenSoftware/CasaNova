<script setup lang="ts">
// SearchBar — the big centered search input from the CasaOS dashboard.
//
// CasaOS's version submits to a configurable search engine (DuckDuckGo
// by default, Google / Bing / etc. as options). For Stage 3 we hard-code
// DuckDuckGo so the UI is functional from day one, with a comment
// marking the integration point for Stage 5 when the user settings API
// lands and provides the real engine choice.
//
// The form opens a new tab rather than navigating the current one —
// nobody wants to lose their dashboard state just because they typed
// into the search bar.

import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import { buildSearchUrl } from '@/composables/useUserPrefs'

const { t } = useI18n()
const query = ref('')

// The search engine choice lives in useUserPrefs and is edited from
// General → Search engine. buildSearchUrl reads the current value and
// returns the fully-encoded URL — this component stays engine-agnostic.
function onSubmit() {
  const q = query.value.trim()
  if (!q) return
  window.open(buildSearchUrl(q), '_blank', 'noopener,noreferrer')
  query.value = ''
}
</script>

<template>
  <form @submit.prevent="onSubmit">
    <label class="cn-widget flex items-center gap-3 px-5 py-3 focus-within:ring-2 focus-within:ring-blue-500/50 transition-shadow">
      <AppIcon name="Search" :size="20" class="flex-shrink-0 text-[var(--cn-text-muted)]" />
      <input
        v-model="query"
        type="search"
        :placeholder="t('dashboard.search_placeholder')"
        class="w-full bg-transparent text-base text-[var(--cn-text)] placeholder:text-[var(--cn-text-muted)] focus:outline-none"
      />
    </label>
  </form>
</template>
