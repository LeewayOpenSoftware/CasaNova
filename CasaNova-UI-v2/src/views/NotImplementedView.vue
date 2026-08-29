<script setup lang="ts">
// NotImplementedView — reusable terminal-styled placeholder for features
// that are on the roadmap but not yet built. CasaNova leans into the
// "command not found" metaphor because it's honest about the state
// (nothing pretends to work) and on-brand for a Linux-rooted home-server
// OS.
//
// Each stub route passes props that customize the rendered command,
// hint text, and roadmap-link label. Adding a new stub feature is a
// one-liner in the router — no boilerplate view per feature.
//
// Visual design: dark terminal card with monospace type, blinking
// cursor, subtle green accent matching a real Linux shell. Sits inside
// the same AppLayout as every other authenticated page so the user
// doesn't lose the top bar / notification sidebar context.

import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import Button from 'primevue/button'

interface Props {
  /**
   * The command-like string shown after the $ prompt. Example values:
   *   "casanova vms"
   *   "casanova backup mount"
   *   "casanova network tailscale"
   * Kept short so it reads as a single fluent command. Rendered verbatim.
   */
  command: string
  /**
   * Short human-readable hint paragraph. Takes plain text (no markup)
   * because it's shown inside the terminal card body.
   */
  hint?: string
  /**
   * Optional link to the roadmap entry or tracking issue for this
   * feature. Opens in a new tab. Omit when there's nothing to link to
   * and the "Learn more" button hides automatically.
   */
  roadmapUrl?: string
}

const props = withDefaults(defineProps<Props>(), {
  hint: '',
  roadmapUrl: '',
})

const { t } = useI18n()
const route = useRoute()
const router = useRouter()

function goBack() {
  // Prefer history.back() so the user returns to wherever they were —
  // usually the dashboard app grid or settings nav. Fall back to /app
  // when there's no history (direct link from outside).
  if (window.history.length > 1) {
    router.back()
  } else {
    router.push('/app')
  }
}

function openRoadmap() {
  if (props.roadmapUrl) {
    window.open(props.roadmapUrl, '_blank', 'noopener,noreferrer')
  }
}

// Page title comes from the route's meta.title if set, else a generic
// fallback. Router guards stash titles per-route in the route table.
const pageTitle = (route.meta.title as string) || t('not_implemented.title')
</script>

<template>
  <div class="mx-auto flex min-h-full max-w-3xl flex-col p-6">
    <!-- Breadcrumb-ish back affordance at the top of the page, matching
         the CasaOS modal back-arrow pattern. -->
    <button
      class="mb-4 inline-flex items-center gap-2 self-start text-sm text-[var(--cn-text-muted)] transition-colors hover:text-[var(--cn-text)]"
      @click="goBack"
    >
      <AppIcon name="ArrowLeft" :size="16" />
      {{ t('not_implemented.back') }}
    </button>

    <h1 class="mb-6 text-2xl font-semibold text-[var(--cn-text)]">{{ pageTitle }}</h1>

    <!-- Terminal card. Monospace, dark fill, green prompt — the three
         cues that immediately read as "shell session" to anyone
         familiar with a Linux terminal. Kept simple: no fake tabs,
         no fake toolbar, nothing that suggests this is a real REPL. -->
    <div
      class="rounded-xl border border-[var(--cn-border)] bg-[#0b0e14] p-6 font-mono text-sm shadow-lg"
    >
      <div class="mb-2 flex items-center gap-2 text-xs text-slate-500">
        <span class="h-3 w-3 rounded-full bg-red-500/80" />
        <span class="h-3 w-3 rounded-full bg-yellow-500/80" />
        <span class="h-3 w-3 rounded-full bg-emerald-500/80" />
        <span class="ml-2">~/casanova</span>
      </div>

      <div class="mt-4">
        <!-- Command line: green $ prompt + typed command. -->
        <div class="text-slate-200">
          <span class="text-emerald-400">$</span>
          <span class="ml-2">{{ command }}</span>
        </div>

        <!-- Error line: mimics bash's actual "command not found" output
             verbatim so the metaphor stays tight. -->
        <div class="mt-1 text-slate-400">
          bash: {{ command }}: {{ t('not_implemented.command_not_found') }}
        </div>

        <!-- Hint paragraph: muted color so it reads as shell-emitted
             helper output rather than a chrome element. -->
        <div v-if="hint" class="mt-4 text-slate-300">{{ hint }}</div>

        <!-- Blinking cursor at the end to communicate "interactive
             session waiting for input" without implying this is a real
             terminal. Pure CSS animation via the step-end keyframes. -->
        <div class="mt-6 text-slate-200">
          <span class="text-emerald-400">$</span>
          <span class="ml-2 inline-block h-4 w-2 animate-pulse bg-slate-300 align-middle" />
        </div>
      </div>
    </div>

    <!-- Action row. Back returns to prior page; Roadmap link opens
         the tracker in a new tab when provided. -->
    <div class="mt-6 flex items-center gap-3">
      <Button severity="secondary" @click="goBack">
        {{ t('not_implemented.back') }}
      </Button>
      <Button v-if="roadmapUrl" @click="openRoadmap">
        <template #icon>
          <AppIcon name="ExternalLink" :size="16" />
        </template>
        {{ t('not_implemented.view_roadmap') }}
      </Button>
    </div>
  </div>
</template>
