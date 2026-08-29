<script setup lang="ts">
// Notes — a persistent scratchpad that lives on the server.
//
// WHAT THIS IS FOR, because it shapes every decision below.
//
// The driving use case is not "writing documents". It is: type a command on one
// machine, read and run it on another. RustDesk and the like move a screen, not
// a line of text you can paste. So the things that matter are, in order:
//
//   1. Copy — one click, from any machine, no selection fiddling. It is the
//      most prominent control in the editor for exactly that reason.
//   2. Save and reload — deliberately not live-syncing. Two machines editing
//      one note through a socket needs conflict resolution; "save here, refresh
//      there" needs nothing and cannot corrupt anything.
//   3. Rendering — useful for checklists and reminders, secondary to the above.
//
// The notes are .md files under /DATA/Notes. They are visible in the File
// Manager, included in backups, and a real Obsidian vault if you point one at
// that folder. No database, nothing that only CasaNova can read.

import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import { useToast } from 'primevue/usetoast'
import AppIcon from '@/components/ui/AppIcon.vue'
import {
  NOTES_ROOT,
  createNote,
  NoteExistsError,
  deleteNote,
  listNotes,
  readNote,
  writeNote,
  type NoteMeta,
} from '@/api/notes'
import { backendMessage } from '@/api/files'

const { t } = useI18n()
const toast = useToast()

const notes = ref<NoteMeta[]>([])
const active = ref<NoteMeta | null>(null)
const body = ref('')
/** What was last read from or written to disk — the baseline "dirty" is measured
 * against. Without it, opening a note would immediately look unsaved. */
const savedBody = ref('')
const loading = ref(false)
const saving = ref(false)
const showPreview = ref(false)
const newTitle = ref('')
const creating = ref(false)
const filter = ref('')
const editorEl = ref<HTMLTextAreaElement | null>(null)

const dirty = computed(() => active.value !== null && body.value !== savedBody.value)

const visibleNotes = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return notes.value
  return notes.value.filter((n) => n.title.toLowerCase().includes(q))
})

// Rendered markdown, sanitized.
//
// The notes are files on disk, so anything with write access to /DATA/Notes can
// author what lands here — a sync client, a script, a restored backup. Rendering
// that straight into v-html would make "someone wrote a file" into "someone ran
// script in your session". DOMPurify is what makes the preview safe to keep.
const rendered = computed(() => {
  const raw = marked.parse(body.value || '', { async: false }) as string
  return DOMPurify.sanitize(raw)
})

function fmtWhen(ms: number): string {
  if (!ms) return ''
  const diff = Date.now() - ms
  const min = Math.round(diff / 60000)
  if (min < 1) return t('notes.just_now')
  if (min < 60) return t('notes.minutes_ago', { n: min }, min)
  const hrs = Math.round(min / 60)
  if (hrs < 24) return t('notes.hours_ago', { n: hrs }, hrs)
  return new Date(ms).toLocaleDateString()
}

// The server's message names the actual problem — a missing file, a refused
// path, the size cap. Axios only ever says "Request failed with status code N",
// which reads the same for all of them.
function errorDetail(e: unknown): string {
  return backendMessage(e) ?? (e as Error)?.message ?? ''
}

async function refresh() {
  loading.value = true
  try {
    notes.value = await listNotes()
  } catch (e) {
    toast.add({
      severity: 'error',
      summary: t('notes.load_failed'),
      detail: errorDetail(e),
      life: 6000,
    })
  } finally {
    loading.value = false
  }
}

async function open(n: NoteMeta) {
  if (dirty.value && !confirm(t('notes.discard_confirm'))) return
  try {
    const text = await readNote(n.path)
    active.value = n
    body.value = text
    savedBody.value = text
    showPreview.value = false
  } catch (e) {
    toast.add({
      severity: 'error',
      summary: t('notes.open_failed'),
      detail: errorDetail(e),
      life: 6000,
    })
  }
}

async function save() {
  if (!active.value || saving.value) return
  // The Save button is disabled when nothing changed, but Ctrl+S calls this
  // directly and would sail past that. A write with nothing to write is at
  // best a wasted round-trip and at worst overwrites the file with whatever
  // the buffer holds — so the guard belongs here, not only on the button.
  if (!dirty.value) return
  // A buffer that is not a string means the note never loaded. Writing that
  // back is how an editor turns a failed read into a truncated file.
  if (typeof body.value !== 'string') return
  saving.value = true
  const snapshot = body.value
  try {
    const { recreated } = await writeNote(active.value.path, snapshot)
    if (recreated) {
      toast.add({
        severity: 'info',
        summary: t('notes.recreated_title'),
        detail: t('notes.recreated_detail'),
        life: 7000,
      })
    }
    // Compare against the snapshot, not body.value: the user may have kept
    // typing while the request was in flight, and marking those keystrokes as
    // saved would lose them silently on the next navigation.
    savedBody.value = snapshot
    await refresh()
  } catch (e) {
    toast.add({
      severity: 'error',
      summary: t('notes.save_failed'),
      detail: errorDetail(e),
      life: 8000,
    })
  } finally {
    saving.value = false
  }
}

async function create() {
  const title = newTitle.value.trim()
  if (!title || creating.value) return
  creating.value = true
  try {
    const n = await createNote(title, '')
    newTitle.value = ''
    await refresh()
    const fresh = notes.value.find((x) => x.path === n.path) ?? n
    await open(fresh)
    await nextTick()
    editorEl.value?.focus()
  } catch (e) {
    // A taken name is a normal thing to do by accident, not a failure of the
    // system — say which name and let the user retype, instead of surfacing a
    // raw error string.
    if (e instanceof NoteExistsError) {
      toast.add({
        severity: 'warn',
        summary: t('notes.exists_title'),
        detail: t('notes.exists_detail', { name: e.noteName }),
        life: 5000,
      })
      return
    }
    toast.add({
      severity: 'error',
      summary: t('notes.create_failed'),
      detail: errorDetail(e),
      life: 6000,
    })
  } finally {
    creating.value = false
  }
}

async function remove(n: NoteMeta) {
  // Deleting the open note also throws away whatever is unsaved in it, and the
  // plain confirm asks only about the file. Someone agreeing to delete a note
  // is not thereby agreeing to lose the command they just typed into it — the
  // unsaved text is the whole reason this feature exists, and unlike the file
  // (which goes to the trash) it has no undo.
  const losesEdits = active.value?.path === n.path && dirty.value
  const question = losesEdits
    ? t('notes.delete_confirm_dirty', { name: n.title })
    : t('notes.delete_confirm', { name: n.title })
  if (!confirm(question)) return
  try {
    await deleteNote(n.path)
    if (active.value?.path === n.path) {
      active.value = null
      body.value = ''
      savedBody.value = ''
    }
    await refresh()
  } catch (e) {
    toast.add({
      severity: 'error',
      summary: t('notes.delete_failed'),
      detail: errorDetail(e),
      life: 6000,
    })
  }
}

// THE FEATURE THIS EXISTS FOR.
//
// navigator.clipboard needs a secure context; over plain HTTP on a LAN address
// it is simply undefined. Falling back to the legacy execCommand path keeps the
// one button that justifies the whole section working on the setup most people
// actually have — a NAS on http://192.168.x.x.
async function copyAll() {
  const text = body.value
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text)
    } else {
      const ta = document.createElement('textarea')
      ta.value = text
      ta.style.position = 'fixed'
      ta.style.opacity = '0'
      document.body.appendChild(ta)
      ta.select()
      document.execCommand('copy')
      ta.remove()
    }
    toast.add({ severity: 'success', summary: t('notes.copied'), life: 2000 })
  } catch {
    toast.add({ severity: 'warn', summary: t('notes.copy_failed'), life: 5000 })
  }
}

// Ctrl/Cmd+S saves. Anyone who writes a command into a text box will reach for
// it, and the browser's "save page" dialog is a hostile answer.
function onKeydown(e: KeyboardEvent) {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
    e.preventDefault()
    void save()
  }
}

// Leaving with unsaved text is the one way this feature can lose work.
watch(dirty, (isDirty) => {
  const handler = (e: BeforeUnloadEvent) => {
    e.preventDefault()
    e.returnValue = ''
  }
  if (isDirty) window.addEventListener('beforeunload', handler)
  else window.removeEventListener('beforeunload', handler)
})

onMounted(refresh)
</script>

<template>
  <div class="flex h-full flex-col gap-4 p-4" @keydown="onKeydown">
    <!-- Header -->
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div>
        <h1 class="text-xl font-semibold text-[var(--cn-text)]">{{ t('notes.title') }}</h1>
        <p class="text-xs text-[var(--cn-text-muted)]">
          {{ t('notes.subtitle', { path: NOTES_ROOT }) }}
        </p>
      </div>
      <Button text rounded severity="secondary" :aria-label="t('notes.refresh')" @click="refresh">
        <AppIcon name="RefreshCw" :size="18" />
      </Button>
    </div>

    <div class="grid min-h-0 flex-1 grid-cols-1 gap-4 md:grid-cols-[minmax(220px,300px)_1fr]">
      <!-- Note list -->
      <aside class="cn-widget flex min-h-0 flex-col gap-2 p-3">
        <div class="flex gap-2">
          <InputText
            v-model="newTitle"
            class="min-w-0 flex-1"
            :placeholder="t('notes.new_placeholder')"
            @keydown.enter="create"
          />
          <Button
            :disabled="!newTitle.trim() || creating"
            :aria-label="t('notes.create')"
            @click="create"
          >
            <AppIcon name="Plus" :size="16" />
          </Button>
        </div>

        <InputText v-model="filter" :placeholder="t('notes.filter')" class="w-full" />

        <div class="min-h-0 flex-1 overflow-y-auto">
          <p v-if="loading" class="p-2 text-xs text-[var(--cn-text-muted)]">
            {{ t('common.loading') }}
          </p>
          <p
            v-else-if="visibleNotes.length === 0"
            class="p-2 text-xs text-[var(--cn-text-muted)]"
          >
            {{ filter ? t('notes.none_match') : t('notes.empty') }}
          </p>
          <ul v-else class="flex flex-col gap-1">
            <li v-for="n in visibleNotes" :key="n.path">
              <div
                class="group flex cursor-pointer items-center gap-2 rounded-[var(--cn-radius-sm)] px-2 py-2 transition-colors"
                :class="active?.path === n.path
                  ? 'bg-[var(--cn-accent-soft)] text-[var(--cn-text)]'
                  : 'hover:bg-[var(--cn-surface-hover)]'"
                @click="open(n)"
              >
                <AppIcon name="FileText" :size="16" class="shrink-0 opacity-70" />
                <div class="min-w-0 flex-1">
                  <div class="truncate text-sm">{{ n.title }}</div>
                  <div class="text-[11px] text-[var(--cn-text-muted)]">{{ fmtWhen(n.modified) }}</div>
                </div>
                <button
                  class="shrink-0 opacity-0 transition-opacity group-hover:opacity-70 hover:!opacity-100"
                  :aria-label="t('notes.delete')"
                  @click.stop="remove(n)"
                >
                  <AppIcon name="Trash2" :size="15" />
                </button>
              </div>
            </li>
          </ul>
        </div>
      </aside>

      <!-- Editor -->
      <section class="cn-widget flex min-h-0 flex-col p-3">
        <div v-if="!active" class="flex flex-1 items-center justify-center">
          <p class="text-sm text-[var(--cn-text-muted)]">{{ t('notes.pick_one') }}</p>
        </div>

        <template v-else>
          <div class="mb-3 flex flex-wrap items-center gap-2">
            <span class="min-w-0 flex-1 truncate text-sm font-medium text-[var(--cn-text)]">
              {{ active.title }}
              <span v-if="dirty" class="ml-1 text-[var(--cn-text-muted)]">•</span>
            </span>

            <!-- Copy is first and labelled: it is the reason this section exists. -->
            <Button size="small" severity="secondary" @click="copyAll">
              <AppIcon name="Copy" :size="15" class="mr-1" />
              {{ t('notes.copy') }}
            </Button>
            <Button
              size="small"
              severity="secondary"
              :outlined="!showPreview"
              @click="showPreview = !showPreview"
            >
              <AppIcon :name="showPreview ? 'Pencil' : 'Eye'" :size="15" class="mr-1" />
              {{ showPreview ? t('notes.edit') : t('notes.preview') }}
            </Button>
            <Button size="small" :disabled="!dirty || saving" @click="save">
              {{ saving ? t('notes.saving') : t('notes.save') }}
            </Button>
          </div>

          <textarea
            v-if="!showPreview"
            ref="editorEl"
            v-model="body"
            spellcheck="false"
            class="min-h-0 w-full flex-1 resize-none rounded-[var(--cn-radius-sm)] border border-[var(--cn-border)] bg-[var(--cn-bg)] p-3 font-mono text-sm leading-relaxed text-[var(--cn-text)] outline-none focus:border-[var(--cn-accent)]"
            :placeholder="t('notes.editor_placeholder')"
          />
          <!-- eslint-disable-next-line vue/no-v-html -- sanitized above with DOMPurify -->
          <div
            v-else
            class="cn-note-preview min-h-0 flex-1 overflow-y-auto rounded-[var(--cn-radius-sm)] border border-[var(--cn-border)] p-3 text-sm"
            v-html="rendered"
          />
        </template>
      </section>
    </div>
  </div>
</template>

<style scoped>
/* Minimal prose styling for the preview — enough that headings, lists and code
 * are legible, without pulling in a typography plugin for one view. */
.cn-note-preview :deep(h1),
.cn-note-preview :deep(h2),
.cn-note-preview :deep(h3) {
  font-weight: 600;
  margin: 0.8em 0 0.4em;
  line-height: 1.25;
}
.cn-note-preview :deep(h1) { font-size: 1.35em; }
.cn-note-preview :deep(h2) { font-size: 1.18em; }
.cn-note-preview :deep(h3) { font-size: 1.05em; }
.cn-note-preview :deep(p) { margin: 0.5em 0; }
.cn-note-preview :deep(ul),
.cn-note-preview :deep(ol) { margin: 0.5em 0; padding-left: 1.4em; }
.cn-note-preview :deep(ul) { list-style: disc; }
.cn-note-preview :deep(ol) { list-style: decimal; }
.cn-note-preview :deep(code) {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 0.9em;
  background: var(--cn-surface-hover);
  padding: 0.1em 0.35em;
  border-radius: 4px;
}
.cn-note-preview :deep(pre) {
  background: var(--cn-surface-hover);
  padding: 0.75em;
  border-radius: var(--cn-radius-sm);
  overflow-x: auto;
  margin: 0.6em 0;
}
.cn-note-preview :deep(pre code) { background: none; padding: 0; }
.cn-note-preview :deep(a) { color: var(--cn-accent); text-decoration: underline; }
.cn-note-preview :deep(blockquote) {
  border-left: 3px solid var(--cn-border);
  padding-left: 0.8em;
  margin: 0.6em 0;
  color: var(--cn-text-muted);
}
.cn-note-preview :deep(table) { border-collapse: collapse; margin: 0.6em 0; }
.cn-note-preview :deep(th),
.cn-note-preview :deep(td) {
  border: 1px solid var(--cn-border);
  padding: 0.35em 0.6em;
}
</style>
