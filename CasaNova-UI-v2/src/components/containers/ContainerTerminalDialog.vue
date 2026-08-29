<script setup lang="ts">
// ContainerTerminalDialog — per-container terminal + log viewer.
//
// Opens either an interactive shell (`docker exec`) or a read-only log
// stream (`docker logs -f`) for a specific container. Uses the same
// xterm.js infrastructure as the system TerminalDialog.vue but connects
// to the per-container WebSocket endpoints instead of SSH.
//
// Props:
//   visible (v-model)  — dialog open/close state
//   containerId        — Docker container ID or name
//   containerName      — Human-readable name for the title
//   mode               — initial tab: 'terminal' | 'logs'

import { ref, computed, nextTick, watch, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import AppIcon from '@/components/ui/AppIcon.vue'
import { containerTerminalWsUrl, containerLogsWsUrl } from '@/api/containers'

const props = withDefaults(
  defineProps<{
    visible: boolean
    containerId: string
    containerName: string
    mode?: 'terminal' | 'logs'
  }>(),
  { mode: 'terminal' },
)
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const { t } = useI18n()

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

type Tab = 'terminal' | 'logs'
const activeTab = ref<Tab>(props.mode)

// ----- Connection state -----
type ConnState = 'connecting' | 'connected' | 'disconnected'
const connState = ref<ConnState>('disconnected')

const connLabel = computed(() => {
  switch (connState.value) {
    case 'connecting':
      return t('container.connecting')
    case 'connected':
      return t('container.connected')
    case 'disconnected':
      return t('container.disconnected')
  }
})

const connDotClass = computed(() => {
  switch (connState.value) {
    case 'connecting':
      return 'bg-yellow-500 animate-pulse'
    case 'connected':
      return 'bg-emerald-500'
    case 'disconnected':
      return 'bg-slate-500'
  }
})

// ----- xterm.js state -----
const xtermRef = ref<HTMLDivElement | null>(null)
let xtermInstance: any = null
let wsConnection: WebSocket | null = null
let xtermResizeObserver: ResizeObserver | null = null

// Wave 31: track any pending init setTimeout so a rapid sequence of
// reconnect()/tab-switch/dialog-toggle calls cannot stack overlapping
// initXterm runs. Without this, two clicks on "reconnect" within 100ms
// schedule two terminals on the same DOM node — one of them stays
// orphaned with a live WebSocket consuming docker exec resources until
// the entire dialog is unmounted.
let pendingInit: ReturnType<typeof setTimeout> | null = null
function scheduleInit(tab: Tab, delay: number) {
  if (pendingInit !== null) {
    clearTimeout(pendingInit)
    pendingInit = null
  }
  pendingInit = setTimeout(() => {
    pendingInit = null
    initXterm(tab)
  }, delay)
}

async function initXterm(mode: Tab) {
  const container = xtermRef.value
  if (!container) return

  // Loading xterm is three dynamic imports. Closing the dialog during them
  // ran cleanupXterm() against nothing, and then this function carried on and
  // opened a WebSocket — a live `docker exec` session (or host PTY) with no
  // reference left to close it. Take a ticket and check it after every await.
  const generation = ++initGeneration

  const { Terminal } = await import('@xterm/xterm')
  const { FitAddon } = await import('@xterm/addon-fit')
  await import('@xterm/xterm/css/xterm.css')

  if (generation !== initGeneration || !props.visible) return

  // Clean up previous instance
  cleanupXterm()
  initGeneration = generation

  const isLogs = mode === 'logs'

  const term = new Terminal({
    cursorBlink: !isLogs,
    disableStdin: isLogs,
    fontSize: 13,
    fontFamily: '"JetBrains Mono", "Fira Code", "Cascadia Code", monospace',
    theme: {
      background: '#0f172a',
      foreground: '#e2e8f0',
      cursor: '#60a5fa',
      selectionBackground: '#334155',
    },
    rows: 24,
    cols: 80,
  })

  const fitAddon = new FitAddon()
  term.loadAddon(fitAddon)
  term.open(container)
  fitAddon.fit()
  xtermInstance = term
  // fitAddon is kept alive by the term instance; no separate ref needed.

  connState.value = 'connecting'

  const wsUrl =
    mode === 'terminal'
      ? containerTerminalWsUrl(props.containerId)
      : containerLogsWsUrl(props.containerId)

  const ws = new WebSocket(wsUrl)
  wsConnection = ws

  ws.onopen = () => {
    connState.value = 'connected'
    term.focus()
  }

  ws.onmessage = (event) => {
    term.write(event.data)
  }

  ws.onerror = () => {
    term.write('\r\n\x1b[31mWebSocket connection error\x1b[0m\r\n')
  }

  ws.onclose = () => {
    connState.value = 'disconnected'
    term.write('\r\n\x1b[33mConnection closed\x1b[0m\r\n')
  }

  // For terminal mode: send input directly (raw bytes, not JSON —
  // the container exec handler reads raw stdin)
  if (!isLogs) {
    term.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(data)
      }
    })
  }

  // Fit on container resize
  const resizeObserver = new ResizeObserver(() => {
    try {
      fitAddon.fit()
    } catch {}
  })
  resizeObserver.observe(container)
  xtermResizeObserver = resizeObserver
}

// Bumped by every init attempt and by every teardown, so an in-flight init
// can tell that the world moved on while it was awaiting its imports.
let initGeneration = 0

function cleanupXterm() {
  initGeneration++
  // Wave 31: cancel any pending scheduled init so a queued initXterm
  // cannot fire after we've already torn down the previous instance.
  if (pendingInit !== null) {
    clearTimeout(pendingInit)
    pendingInit = null
  }
  if (xtermResizeObserver) {
    xtermResizeObserver.disconnect()
    xtermResizeObserver = null
  }
  if (wsConnection) {
    wsConnection.close()
    wsConnection = null
  }
  if (xtermInstance) {
    xtermInstance.dispose()
    xtermInstance = null
  }
  // fitAddon is disposed with the terminal instance.
  connState.value = 'disconnected'
}

function reconnect() {
  cleanupXterm()
  nextTick(() => {
    scheduleInit(activeTab.value, 100)
  })
}

function clearTerminal() {
  if (xtermInstance) {
    xtermInstance.clear()
  }
}

// Start terminal when dialog opens
watch(
  () => props.visible,
  (v) => {
    if (v) {
      activeTab.value = props.mode
      nextTick(() => {
        scheduleInit(activeTab.value, 200)
      })
    } else {
      cleanupXterm()
    }
  },
)

// Switch connection when tab changes
watch(
  () => activeTab.value,
  (tab) => {
    cleanupXterm()
    nextTick(() => {
      scheduleInit(tab, 100)
    })
  },
)

onBeforeUnmount(() => {
  cleanupXterm()
})
</script>

<template>
  <Dialog
    v-model:visible="dialogVisible"
    modal
    :style="{ width: '54rem', height: '34rem' }"
    :dismissable-mask="true"
    :show-header="false"
    content-class="!p-0 !h-full"
  >
    <div
      class="flex h-full flex-col overflow-hidden rounded-[inherit] bg-[#0b0f1a] text-emerald-400"
    >
      <!-- Header with tab strip + container name + status -->
      <div class="flex items-center justify-between border-b border-slate-800 bg-[#0f1420] px-4">
        <div class="flex items-center gap-4">
          <button
            type="button"
            :class="[
              'inline-flex items-center gap-2 border-b-2 py-2 text-xs transition-colors',
              activeTab === 'terminal'
                ? 'border-blue-500 text-slate-100'
                : 'border-transparent text-slate-500 hover:text-slate-300',
            ]"
            @click.stop="activeTab = 'terminal'"
          >
            <AppIcon name="Terminal" :size="13" />
            <span>{{ t('container.terminal') }}</span>
          </button>
          <button
            type="button"
            :class="[
              'inline-flex items-center gap-2 border-b-2 py-2 text-xs transition-colors',
              activeTab === 'logs'
                ? 'border-blue-500 text-slate-100'
                : 'border-transparent text-slate-500 hover:text-slate-300',
            ]"
            @click.stop="activeTab = 'logs'"
          >
            <AppIcon name="FileText" :size="13" />
            <span>{{ t('container.logs') }}</span>
          </button>

          <!-- Container name + connection state -->
          <span class="ml-2 text-xs text-slate-500">
            {{ containerName }}
          </span>
          <span class="inline-flex items-center gap-1.5 text-[10px] text-slate-500">
            <span :class="['inline-block h-1.5 w-1.5 rounded-full', connDotClass]" />
            {{ connLabel }}
          </span>
        </div>

        <div class="flex items-center gap-2">
          <!-- Clear button -->
          <button
            type="button"
            class="flex h-6 w-6 items-center justify-center rounded-md text-slate-400 hover:bg-slate-800 hover:text-slate-100"
            :title="t('container.clear')"
            @click.stop="clearTerminal"
          >
            <AppIcon name="Trash2" :size="12" />
          </button>
          <!-- Reconnect button -->
          <button
            type="button"
            class="flex h-6 w-6 items-center justify-center rounded-md text-slate-400 hover:bg-slate-800 hover:text-slate-100"
            :title="t('container.reconnect')"
            @click.stop="reconnect"
          >
            <AppIcon name="RotateCw" :size="12" />
          </button>
          <!-- Close button -->
          <button
            type="button"
            class="flex h-6 w-6 items-center justify-center rounded-md text-slate-400 hover:bg-slate-800 hover:text-slate-100"
            :aria-label="t('common.close')"
            @click.stop="dialogVisible = false"
          >
            <AppIcon name="X" :size="14" />
          </button>
        </div>
      </div>

      <!-- Terminal surface -->
      <div class="flex flex-1 flex-col overflow-hidden">
        <div
          ref="xtermRef"
          class="flex-1 bg-[#0f172a]"
        />
      </div>
    </div>
  </Dialog>
</template>
