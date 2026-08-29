<script setup lang="ts">
// GlobalSearch — command-palette-style search across ALL storage roots,
// backed by casanova-search (POST /v2/search/file). Distinct from the file
// browser's in-folder search: this finds files anywhere under /DATA, /mnt,
// /media. Opened from the topbar search icon.

import { ref, watch, nextTick, computed } from 'vue'
import { useRouter } from 'vue-router'
import Dialog from 'primevue/dialog'
import AppIcon from '@/components/ui/AppIcon.vue'
import { globalSearch, type SearchHit } from '@/api/files'
import { useFilesStore } from '@/stores/files'

const props = defineProps<{ visible: boolean }>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const router = useRouter()
const files = useFilesStore()

const model = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

const keyword = ref('')
const hits = ref<SearchHit[]>([]) // "in this folder" when scoped, else all results
const moreHits = ref<SearchHit[]>([]) // "everywhere else" (only when scoped)
const total = ref(0)
const took = ref(0)
const truncated = ref(false)
const loading = ref(false)
const searched = ref(false)
const inputEl = ref<HTMLInputElement | null>(null)

// When the file manager is open on a folder under a storage root, search
// prioritises THAT folder first, then broadens to the whole box. Empty when
// we're not in a scoped location (search is purely global).
const scopeDir = computed(() => {
  const p = files.currentPath
  if (!p) return ''
  for (const r of ['/DATA', '/mnt', '/media']) {
    if (p === r || p.startsWith(r + '/')) return p
  }
  return ''
})
const scopeLabel = computed(() => {
  const p = scopeDir.value
  if (!p) return ''
  return p.split('/').filter(Boolean).pop() || p
})
const hitKey = (h: SearchHit) => h._source.dir + '/' + h._source.name

// Grouped result sections. When scoped, "In <folder>" comes first, then
// "Everywhere else"; otherwise a single unlabelled list.
const sections = computed<{ label: string; items: SearchHit[] }[]>(() => {
  if (scopeDir.value && (hits.value.length || moreHits.value.length)) {
    const out: { label: string; items: SearchHit[] }[] = []
    if (hits.value.length) out.push({ label: `In ${scopeLabel.value}`, items: hits.value })
    if (moreHits.value.length) out.push({ label: 'Everywhere else', items: moreHits.value })
    return out
  }
  return hits.value.length ? [{ label: '', items: hits.value }] : []
})

let debounce: ReturnType<typeof setTimeout> | null = null
let seq = 0

// Focus the field + reset when the dialog opens.
watch(
  () => props.visible,
  (v) => {
    if (v) {
      nextTick(() => inputEl.value?.focus())
    } else {
      seq++ // invalidate any in-flight search so it can't repopulate on reopen
      keyword.value = ''
      hits.value = []
      moreHits.value = []
      searched.value = false
    }
  },
)

watch(keyword, (q) => {
  if (debounce) clearTimeout(debounce)
  const trimmed = q.trim()
  if (trimmed.length < 2) {
    seq++ // invalidate any in-flight search so a late reply can't repopulate after clear
    hits.value = []
    moreHits.value = []
    searched.value = false
    loading.value = false
    return
  }
  loading.value = true
  debounce = setTimeout(() => runSearch(trimmed), 250)
})

async function runSearch(q: string) {
  const mine = ++seq
  loading.value = true
  try {
    const scope = scopeDir.value
    if (scope) {
      // Two-tier: the current folder first, then everywhere else (deduped).
      const [scoped, global] = await Promise.all([
        globalSearch(q, { dir: scope, count: 40 }),
        globalSearch(q, { count: 60 }),
      ])
      if (mine !== seq) return
      const scopedKeys = new Set(scoped.data.hits.map(hitKey))
      hits.value = scoped.data.hits
      moreHits.value = global.data.hits.filter((h) => !scopedKeys.has(hitKey(h)))
      total.value = hits.value.length + moreHits.value.length
      took.value = Math.max(scoped.took, global.took)
      truncated.value = scoped.timed_out || global.timed_out
    } else {
      const res = await globalSearch(q, { count: 60 })
      if (mine !== seq) return
      hits.value = res.data.hits
      moreHits.value = []
      total.value = res.data.total
      took.value = res.took
      truncated.value = res.timed_out
    }
    searched.value = true
  } catch {
    if (mine === seq) {
      hits.value = []
      moreHits.value = []
      searched.value = true
    }
  } finally {
    if (mine === seq) loading.value = false
  }
}

function iconFor(type: string): string {
  switch (type) {
    case 'dir':
      return 'Folder'
    case 'doc':
    case 'pdf':
    case 'txt':
      return 'FileText'
    case 'img':
      return 'Image'
    case 'video':
      return 'Film'
    case 'audio':
      return 'Music'
    case 'archive':
      return 'FileArchive'
    default:
      return 'File'
  }
}

function humanSize(bytes: number): string {
  if (bytes <= 0) return ''
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let n = bytes
  let i = 0
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024
    i++
  }
  return `${n.toFixed(n < 10 && i > 0 ? 1 : 0)} ${u[i]}`
}

// Open a hit: navigate the file browser to the containing folder (for a file)
// or into the folder itself (for a directory).
function openHit(hit: SearchHit) {
  const target = hit._source.is_dir
    ? `${hit._source.dir.replace(/\/$/, '')}/${hit._source.name}`
    : hit._source.dir
  files.cd(target)
  model.value = false
  router.push('/app/files')
}
</script>

<template>
  <Dialog
    v-model:visible="model"
    modal
    dismissable-mask
    :show-header="false"
    :style="{ width: '38rem', maxWidth: '92vw' }"
    content-class="!p-0 !rounded-2xl overflow-hidden"
  >
    <!-- Search field -->
    <div class="flex items-center gap-2 border-b border-[var(--cn-border)] px-4 py-3">
      <AppIcon name="Search" :size="18" class="shrink-0 text-[var(--cn-text-muted)]" />
      <input
        ref="inputEl"
        v-model="keyword"
        type="text"
        :placeholder="'Search all files…'"
        class="flex-1 bg-transparent text-base text-[var(--cn-text)] placeholder:text-[var(--cn-text-muted)] focus:outline-none"
        @keyup.escape="model = false"
      />
      <AppIcon v-if="loading" name="Loader2" :size="16" class="shrink-0 animate-spin text-[var(--cn-text-muted)]" />
    </div>

    <!-- Results -->
    <div class="max-h-[24rem] overflow-y-auto">
      <template v-for="section in sections" :key="section.label">
        <div
          v-if="section.label"
          class="sticky top-0 z-10 bg-[var(--cn-surface)] px-4 pb-1 pt-2 text-[11px] font-semibold uppercase tracking-wide text-[var(--cn-text-muted)] backdrop-blur"
        >
          {{ section.label }}
        </div>
        <button
          v-for="hit in section.items"
          :key="hit._id + hit._source.dir + hit._source.name"
          type="button"
          class="flex w-full items-center gap-3 px-4 py-2.5 text-left hover:bg-[var(--cn-surface-hover)]"
          @click="openHit(hit)"
        >
          <AppIcon
            :name="iconFor(hit._source.type)"
            :size="18"
            class="shrink-0"
            :class="hit._source.is_dir ? 'text-blue-500' : 'text-[var(--cn-text-muted)]'"
          />
          <div class="min-w-0 flex-1">
            <!-- hl_name is HTML-escaped server-side; only the <b> match markup is literal -->
            <div class="truncate text-sm text-[var(--cn-text)]" v-html="hit._source.hl_name" />
            <div class="truncate text-xs text-[var(--cn-text-muted)]">{{ hit._source.dir }}</div>
          </div>
          <div v-if="!hit._source.is_dir" class="shrink-0 text-xs text-[var(--cn-text-muted)]">
            {{ humanSize(hit._source.size) }}
          </div>
        </button>
      </template>

      <!-- States -->
      <div
        v-if="searched && !loading && hits.length === 0 && moreHits.length === 0"
        class="px-4 py-10 text-center text-sm text-[var(--cn-text-muted)]"
      >
        No files found for “{{ keyword.trim() }}”.
      </div>
      <div
        v-else-if="!searched && !loading"
        class="px-4 py-10 text-center text-sm text-[var(--cn-text-muted)]"
      >
        <span v-if="scopeLabel">Searching “{{ scopeLabel }}” first, then everywhere.</span>
        <span v-else>Type at least 2 characters to search across your storage.</span>
      </div>
    </div>

    <!-- Footer: result meta -->
    <div
      v-if="searched && hits.length > 0"
      class="flex items-center justify-between border-t border-[var(--cn-border)] px-4 py-2 text-xs text-[var(--cn-text-muted)]"
    >
      <span>{{ total }} result{{ total === 1 ? '' : 's' }}{{ truncated ? '+' : '' }} · {{ took }} ms</span>
      <span>Enter a folder or open a file</span>
    </div>
  </Dialog>
</template>
