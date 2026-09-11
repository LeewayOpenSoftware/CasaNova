// Dashboard widget layout — order + visibility of the left-column widgets,
// plus an edit mode. Persisted per-user via syncedPrefs so a user's dashboard
// arrangement follows them across browsers (same idiom as app order / tips).
import { defineStore } from 'pinia'
import { ref, computed, watch } from 'vue'
import { getPref, setPref, prefsGeneration } from '@/composables/syncedPrefs'

// Every widget the registry knows about. IDs must match registry keys.
export const ALL_WIDGET_IDS = [
  'clock', 'system', 'storage', 'network',
  'vms', 'diskhealth', 'traffic', 'uptime', 'tasks', 'notifications',
] as const
export type WidgetId = (typeof ALL_WIDGET_IDS)[number]

// Default layout = the four that shipped before this feature, in order.
const DEFAULT_ORDER: WidgetId[] = ['clock', 'system', 'storage', 'network']

const ORDER_KEY = 'casanova.widgets.order'
const HIDDEN_KEY = 'casanova.widgets.hidden'

function loadIds(key: string, fallback: WidgetId[]): WidgetId[] {
  try {
    const raw = getPref(key)
    if (!raw) return [...fallback]
    const parsed = JSON.parse(raw) as string[]
    // keep only ids the registry still knows (drop stale ones)
    const valid = parsed.filter((id): id is WidgetId => (ALL_WIDGET_IDS as readonly string[]).includes(id))
    return valid.length || key === HIDDEN_KEY ? valid : [...fallback]
  } catch {
    return [...fallback]
  }
}

export const useWidgetLayoutStore = defineStore('widgetLayout', () => {
  const order = ref<WidgetId[]>(loadIds(ORDER_KEY, DEFAULT_ORDER))
  const hidden = ref<WidgetId[]>(loadIds(HIDDEN_KEY, []))
  const editMode = ref(false)

  // Never re-write a value the cache already holds: adopting a hydrated
  // layout below fires these watchers, which would echo it straight back.
  const persist = (key: string, v: WidgetId[]) => {
    const json = JSON.stringify(v)
    if (getPref(key) !== json) setPref(key, json)
  }
  watch(order, (v) => persist(ORDER_KEY, v), { deep: true })
  watch(hidden, (v) => persist(HIDDEN_KEY, v), { deep: true })

  // Re-read once the backend hydrate lands — see iconPack.ts for why the
  // constructor's values cannot be the last word on a fresh login.
  const gen = prefsGeneration()
  watch(gen, () => {
    order.value = loadIds(ORDER_KEY, DEFAULT_ORDER)
    hidden.value = loadIds(HIDDEN_KEY, [])
  })

  // Widgets to actually render, in order, minus hidden ones.
  const visible = computed<WidgetId[]>(() => order.value.filter((id) => !hidden.value.includes(id)))
  // Widgets the user can add: not in order at all, OR present-but-hidden.
  const addable = computed<WidgetId[]>(() =>
    ALL_WIDGET_IDS.filter((id) => !order.value.includes(id) || hidden.value.includes(id)),
  )

  function toggleEdit() {
    editMode.value = !editMode.value
  }

  function isHidden(id: WidgetId) {
    return hidden.value.includes(id)
  }

  function hide(id: WidgetId) {
    if (!hidden.value.includes(id)) hidden.value = [...hidden.value, id]
  }

  function show(id: WidgetId) {
    hidden.value = hidden.value.filter((x) => x !== id)
    if (!order.value.includes(id)) order.value = [...order.value, id]
  }

  // Add from the picker: append (or unhide) and make visible.
  function add(id: WidgetId) {
    show(id)
  }

  function move(id: WidgetId, dir: -1 | 1) {
    const i = order.value.indexOf(id)
    if (i < 0) return
    const j = i + dir
    if (j < 0 || j >= order.value.length) return
    const next = [...order.value]
    ;[next[i], next[j]] = [next[j], next[i]]
    order.value = next
  }

  function reset() {
    order.value = [...DEFAULT_ORDER]
    hidden.value = []
  }

  return {
    order, hidden, editMode,
    visible, addable,
    toggleEdit, isHidden, hide, show, add, move, reset,
  }
})
