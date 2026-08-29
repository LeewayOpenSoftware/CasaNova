<script setup lang="ts">
// VncConsole — a draggable floating VM console powered by noVNC (RFB) over the
// gateway websockify bridge (CasaNova-Virt /domain/:name/vnc/ws).
//
// It is a self-contained floating window (NOT a PrimeVue modal): a modal's
// focus-trap steals the keyboard from the guest, and its header-only dragging
// forces a second title bar. Here the single toolbar IS the drag handle, and
// the canvas keeps keyboard focus so you can actually type into the VM.
//
// The WebSocket rides the SAME origin as CasaNova (the gateway port), so the
// console works on the LAN and remotely through the tunnel alike.
import { ref, onMounted, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import RFB from '@novnc/novnc'
import Button from 'primevue/button'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useAuthStore } from '@/stores/auth'

const props = defineProps<{ name: string; password?: string }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const { t } = useI18n()
const auth = useAuthStore()

const screen = ref<HTMLDivElement | null>(null)
type Status = 'connecting' | 'connected' | 'disconnected' | 'error'
const status = ref<Status>('connecting')
const errorMsg = ref('')
let rfb: RFB | null = null

// ── drag (the toolbar is the handle) ──────────────────────────────────
const pos = ref({ x: 0, y: 0 }) // offset from centered origin
let dragStart: { mx: number; my: number; px: number; py: number } | null = null

function onDragMove(e: MouseEvent) {
  if (!dragStart) return
  pos.value = { x: dragStart.px + (e.clientX - dragStart.mx), y: dragStart.py + (e.clientY - dragStart.my) }
}
function onDragEnd() {
  dragStart = null
  window.removeEventListener('mousemove', onDragMove)
  window.removeEventListener('mouseup', onDragEnd)
}
function startDrag(e: MouseEvent) {
  // ignore drags that begin on a button (those are controls, not the handle)
  if ((e.target as HTMLElement).closest('button')) return
  dragStart = { mx: e.clientX, my: e.clientY, px: pos.value.x, py: pos.value.y }
  window.addEventListener('mousemove', onDragMove)
  window.addEventListener('mouseup', onDragEnd)
  e.preventDefault()
}

// ── noVNC ─────────────────────────────────────────────────────────────
function wsUrl(): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const token = auth.accessToken || localStorage.getItem('casanova.access_token') || ''
  return `${proto}//${location.host}/v2/virtualization/domain/${encodeURIComponent(props.name)}/vnc/ws?token=${encodeURIComponent(token)}`
}

function connect() {
  if (!screen.value) return
  status.value = 'connecting'
  errorMsg.value = ''
  try {
    rfb = new RFB(screen.value, wsUrl(), props.password ? { credentials: { password: props.password } } : {})
    rfb.scaleViewport = true
    rfb.resizeSession = false
    rfb.background = '#000'
    rfb.addEventListener('connect', () => {
      status.value = 'connected'
      // Grab keyboard focus so keystrokes go to the guest, not the page.
      rfb?.focus()
    })
    rfb.addEventListener('disconnect', (e) => {
      const clean = (e as CustomEvent<{ clean: boolean }>).detail?.clean
      status.value = clean ? 'disconnected' : 'error'
      if (!clean) errorMsg.value = t('vms.console.lost')
    })
    rfb.addEventListener('securityfailure', (e) => {
      status.value = 'error'
      errorMsg.value = (e as CustomEvent<{ reason?: string }>).detail?.reason || t('vms.console.security')
    })
  } catch (e) {
    status.value = 'error'
    errorMsg.value = e instanceof Error ? e.message : String(e)
  }
}

function disconnect() {
  if (rfb) {
    try { rfb.disconnect() } catch { /* already closed */ }
    rfb = null
  }
}
function reconnect() {
  disconnect()
  pos.value = { x: 0, y: 0 }
  connect()
}
function ctrlAltDel() {
  if (status.value === 'connected') rfb?.sendCtrlAltDel()
}
function refocus() {
  if (status.value === 'connected') rfb?.focus()
}
function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape') emit('close')
}

const dotClass = () =>
  status.value === 'connected' ? 'bg-emerald-500'
    : status.value === 'error' ? 'bg-red-500'
      : 'bg-amber-500'

onMounted(() => {
  connect()
  window.addEventListener('keydown', onKeydown)
})
onBeforeUnmount(() => {
  disconnect()
  window.removeEventListener('keydown', onKeydown)
  onDragEnd()
})
</script>

<template>
  <!-- Dim backdrop (no click-to-close so a drag that ends outside the window
       doesn't dismiss it; use the ✕ or Esc). Not a focus-trap → the canvas
       keeps the keyboard. -->
  <div class="fixed inset-0 z-[120] flex items-center justify-center bg-black/55">
    <div
      class="flex max-h-[92vh] w-[90vw] max-w-[1100px] flex-col overflow-hidden rounded-xl border border-[var(--cn-border)] bg-black shadow-2xl"
      :style="{ transform: `translate(${pos.x}px, ${pos.y}px)`, height: '84vh' }"
    >
      <!-- toolbar = drag handle -->
      <div
        class="flex cursor-move select-none items-center justify-between gap-2 border-b border-[var(--cn-border)] bg-[var(--cn-surface)] px-3 py-2"
        @mousedown="startDrag"
      >
        <div class="flex min-w-0 items-center gap-2">
          <AppIcon name="Move" :size="13" class="shrink-0 text-[var(--cn-text-muted)]" />
          <span class="h-2.5 w-2.5 shrink-0 rounded-full" :class="dotClass()" />
          <span class="truncate text-sm font-semibold text-[var(--cn-text)]">{{ name }}</span>
          <span class="truncate text-xs text-[var(--cn-text-muted)]">{{ t(`vms.console.${status}`) }}</span>
        </div>
        <div class="flex shrink-0 items-center gap-1">
          <Button size="small" severity="secondary" :disabled="status !== 'connected'" label="Ctrl+Alt+Supr" @click="ctrlAltDel" />
          <Button size="small" severity="secondary" :title="t('vms.console.reconnect')" @click="reconnect">
            <template #icon><AppIcon name="RotateCw" :size="14" /></template>
          </Button>
          <Button size="small" severity="secondary" :title="t('common.close')" @click="emit('close')">
            <template #icon><AppIcon name="X" :size="14" /></template>
          </Button>
        </div>
      </div>

      <!-- screen — click anywhere to (re)grab keyboard focus for the guest -->
      <div class="relative flex-1" @mousedown="refocus">
        <div ref="screen" class="absolute inset-0" />
        <div
          v-if="status === 'connecting'"
          class="pointer-events-none absolute inset-0 flex items-center justify-center text-sm text-white/70"
        >
          <AppIcon name="RotateCw" :size="18" class="mr-2 animate-spin" /> {{ t('vms.console.connecting') }}
        </div>
        <div
          v-else-if="status === 'error' || status === 'disconnected'"
          class="absolute inset-0 flex flex-col items-center justify-center gap-3 text-center text-sm text-white/80"
        >
          <AppIcon name="MonitorX" :size="28" class="text-white/50" />
          <div>{{ errorMsg || t(`vms.console.${status}`) }}</div>
          <Button size="small" :label="t('vms.console.reconnect')" @click="reconnect">
            <template #icon><AppIcon name="RotateCw" :size="14" /></template>
          </Button>
        </div>
      </div>
    </div>
  </div>
</template>
