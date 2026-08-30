<script setup lang="ts">
// An app icon that survives a URL that does not load.
//
// The Apps section used a bare <img :src> with a v-else letter tile. v-else
// only fires when the URL is ABSENT — a URL that is present and fails (CSP
// blocked it, the host 404s, the box is offline) renders as an empty square
// with no way out. This walks the same candidate list AppCard uses and shows
// the letter only once every one of them has failed.

import { computed, ref, watch } from 'vue'
import { appIconCandidates } from '@/composables/appIconCandidates'

const props = withDefaults(
  defineProps<{
    iconUrl?: string
    id?: string
    name?: string
    /** Tailwind sizing for both the image and the letter tile. */
    sizeClass?: string
    /** Text size for the letter tile; ignored when an icon loads. */
    textClass?: string
  }>(),
  { sizeClass: 'h-12 w-12 rounded-xl', textClass: 'text-lg' },
)

const candidates = computed(() => appIconCandidates(props))
const index = ref(0)
const exhausted = ref(false)

// A different app (or a changed icon) gets its own verdict rather than
// inheriting the previous tile's exhausted state.
watch(candidates, () => {
  index.value = 0
  exhausted.value = false
})

const src = computed(() => (exhausted.value ? '' : (candidates.value[index.value] ?? '')))

function onError() {
  if (index.value + 1 >= candidates.value.length) {
    exhausted.value = true
    return
  }
  index.value += 1
}

const initial = computed(() => (props.name || props.id || '?').trim().charAt(0).toUpperCase() || '?')
</script>

<template>
  <img v-if="src" :src="src" :class="[sizeClass, 'object-contain']" alt="" @error="onError" />
  <div
    v-else
    :class="[
      sizeClass,
      textClass,
      'flex items-center justify-center bg-[var(--cn-surface-hover)] font-semibold text-[var(--cn-text-muted)]',
    ]"
  >
    {{ initial }}
  </div>
</template>
