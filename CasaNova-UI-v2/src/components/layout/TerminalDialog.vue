<script setup lang="ts">
// TerminalDialog — the general system terminal opened from the top bar.
//
// Until we wire a real WebSocket-to-PTY bridge on the backend, this
// runs a small in-browser fake shell so the feature is visible and
// feels alive. The fake shell recognizes a handful of common commands
// (ls, pwd, whoami, uname, echo, cat, clear, help, exit) and answers
// anything else with "command not found". History with up/down arrows
// mirrors bash so muscle memory works.
//
// When the backend ships a real /v2/casaos/terminal/ws endpoint, the
// runCommand() function is the single swap point — it becomes
// `ws.send(line)` and the onmessage handler appends to `lines.value`.
// Every other piece (scroll, history, focus, paste) stays put.

import { ref, computed, nextTick, watch, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import InputNumber from 'primevue/inputnumber'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useThemeStore } from '@/stores/theme'

const theme = useThemeStore()

// Read a CSS variable from <html>'s computed style. Returns the trimmed
// value or the fallback when the var is unset. Needed because xterm.js
// takes a flat object — it can't subscribe to CSS variables itself.
function cssVar(name: string, fallback: string): string {
  if (typeof window === 'undefined') return fallback
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return v || fallback
}

// terminalPalette derives an xterm theme object from the live CSS
// variables. Recomputed every time we (re)initialise the terminal so the
// palette tracks the active theme variant + dark mode + accent color.
function terminalPalette() {
  const isDark = document.documentElement.classList.contains('dark')
  return {
    // Surface tracks the active theme — Longhorn glass, Zethus paper,
    // default dark slate. We pin to a dark-ish color even in light mode
    // because a white terminal background is hard to read.
    background: isDark
      ? cssVar('--cn-surface', '#0f172a')
      : '#0f172a',
    foreground: isDark
      ? cssVar('--cn-text', '#e2e8f0')
      : '#e2e8f0',
    cursor: cssVar('--cn-accent', '#60a5fa'),
    selectionBackground: '#33415588',
  }
}

const props = defineProps<{
  visible: boolean
}>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const { t } = useI18n()

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

// ----- SSH auth state -----
// The terminal gates the shell behind an SSH-style login because the
// system's shell user is often distinct from the dashboard user (root
// vs admin, remote box vs local, etc). The login persists for the
// life of the dialog — closing and reopening brings the user back to
// the login form for a fresh auth.
const authenticated = ref(false)
const sshUsername = ref('')
const sshPassword = ref('')
const sshPort = ref<number | null>(22)
const sshPasswordVisible = ref(false)
const sshError = ref<string | null>(null)
const sshSubmitting = ref(false)
let sshSessionToken = '' // single-use token from POST /v1/sys/ssh-login

// connection drives the chrome state indicator + the Reconnect affordance.
// 'connecting' = WS opening, 'open' = live shell, 'closed' = dropped or
// idle-disconnected (user can hit Reconnect), 'error' = WS errored.
const connection = ref<'idle' | 'connecting' | 'open' | 'closed' | 'error'>('idle')
const connectionMessage = ref('')

const canSubmitSsh = computed(
  () =>
    sshUsername.value.trim().length > 0 &&
    sshPassword.value.length > 0 &&
    sshPort.value !== null &&
    sshPort.value > 0 &&
    sshPort.value < 65536 &&
    !sshSubmitting.value,
)

// Tab strip at the top (Terminal / Logs). CasaOS mirrors this layout;
// logs is a stub placeholder until the backend journald endpoint lands.
type Tab = 'terminal' | 'logs'
const activeTab = ref<Tab>('terminal')

const inputRef = ref<HTMLInputElement | null>(null)

// ----- SSH login handler -----
// Real backend call: POST /v1/sys/ssh-login with { username, password,
// port } → receive a session id → open the WebSocket /v1/sys/wsssh
// with that session. Dev fallback accepts any non-empty credentials
// so the scaffold stays usable offline.
async function submitSsh() {
  if (!canSubmitSsh.value) return
  sshSubmitting.value = true
  sshError.value = null
  let systemApi: typeof import('@/api').systemApi
  let safeCall: typeof import('@/api/safeCall').safeCall
  try {
    ;({ systemApi } = await import('@/api'))
    ;({ safeCall } = await import('@/api/safeCall'))
  } catch {
    sshSubmitting.value = false
    sshError.value = 'Failed to load login'
    return
  }
  const result = await safeCall(
    () =>
      systemApi.sshLogin({
        username: sshUsername.value.trim(),
        password: sshPassword.value,
        port: String(sshPort.value ?? 22),
      }),
  )
  sshSubmitting.value = false
  if (result.error) {
    sshError.value = result.error
    return
  }
  if (!result.data) return
  // Store the session token — credentials stay client-side only for the
  // login POST body. The WebSocket URL receives ONLY the opaque token.
  sshSessionToken = result.data.session_id ?? ''
  authenticated.value = true
  // Open real WebSocket terminal via the tracked scheduler so a rapid
  // login → cancel → login sequence can't queue overlapping initXterm
  // runs on the same DOM node.
  nextTick(() => {
    scheduleInit(200)
  })
}

// Wave 32: track any pending init setTimeout so a rapid auth bounce
// (submit → cancel → submit, dialog close-during-init, repeat clicks)
// cannot stack overlapping initXterm runs. Without this, two queued
// inits race on the same DOM node and one of them strands a live
// WebSocket consuming SSH PTY resources until the dialog is destroyed.
// Mirrors the Wave 31 fix in ContainerTerminalDialog.vue.
let pendingInit: ReturnType<typeof setTimeout> | null = null
function scheduleInit(delay: number) {
  if (pendingInit !== null) {
    clearTimeout(pendingInit)
    pendingInit = null
  }
  pendingInit = setTimeout(() => {
    pendingInit = null
    void initXterm().catch(() => { cleanupXterm(); connection.value = 'error'; sshError.value = 'Terminal initialization failed' })
  }, delay)
}

// ----- Real xterm.js WebSocket terminal -----
const xtermRef = ref<HTMLDivElement | null>(null)
let xtermInstance: any = null
let xtermFitAddon: any = null
let wsConnection: WebSocket | null = null

async function initXterm() {
  const container = xtermRef.value
  if (!container) return

  const { Terminal } = await import('@xterm/xterm')
  const { FitAddon } = await import('@xterm/addon-fit')
  // Import xterm CSS
  await import('@xterm/xterm/css/xterm.css')

  // Clean up previous instance
  if (xtermInstance) {
    xtermInstance.dispose()
    xtermInstance = null
  }
  if (wsConnection) {
    wsConnection.close()
    wsConnection = null
  }

  const term = new Terminal({
    cursorBlink: true,
    fontSize: 13,
    fontFamily: '"JetBrains Mono", "Fira Code", "Cascadia Code", monospace',
    theme: terminalPalette(),
    rows: 24,
    cols: 80,
  })

  const fitAddon = new FitAddon()
  term.loadAddon(fitAddon)
  term.open(container)
  fitAddon.fit()
  xtermInstance = term
  xtermFitAddon = fitAddon

  // Build the WebSocket URL for the real SSH PTY.
  // SECURITY: Only the opaque session token travels in the URL — never
  // raw credentials. The token is single-use and expires in 60 seconds.
  // Credentials were validated server-side during POST /v1/sys/ssh-login.
  //
  // Two tokens travel in the URL:
  //   ?jwt=   — the CasaNova access token (JWT) for strictJWT() middleware.
  //             Browsers cannot send custom headers during the WS handshake,
  //             so the JWT rides as a query param instead of Authorization.
  //   ?token= — the opaque SSH session ID consumed by the WsSsh handler.
  const wsProto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const wsHost = window.location.host
  const { useAuthStore } = await import('@/stores/auth')
  const authStore = useAuthStore()
  const params = new URLSearchParams({
    jwt: authStore.accessToken || '',
    token: sshSessionToken,
    cols: String(term.cols),
    rows: String(term.rows),
  })
  const wsUrl = `${wsProto}//${wsHost}/v1/sys/wsssh?${params.toString()}`

  connection.value = 'connecting'
  connectionMessage.value = t('terminal.connecting')

  const ws = new WebSocket(wsUrl)
  wsConnection = ws

  ws.onopen = () => {
    connection.value = 'open'
    connectionMessage.value = ''
    term.focus()
  }

  ws.onmessage = (event) => {
    term.write(event.data)
  }

  ws.onerror = () => {
    connection.value = 'error'
    connectionMessage.value = t('terminal.connection_error')
    term.write('\r\n\x1b[31m' + t('terminal.connection_error') + '\x1b[0m\r\n')
  }

  ws.onclose = () => {
    // Stay open=true once the session has produced output — a quick
    // network blip shouldn't cause Reconnect to flash. Only flag closed
    // when we hadn't already errored.
    if (connection.value !== 'error') {
      connection.value = 'closed'
      connectionMessage.value = t('terminal.connection_closed')
    }
    term.write('\r\n\x1b[33m' + t('terminal.connection_closed') + '\x1b[0m\r\n')
  }

  // Send terminal input to the WebSocket — backend expects JSON
  // messages: {type:"cmd", cmd:"..."} for input, {type:"resize", cols, rows} for resize
  term.onData((data) => {
    if (ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'cmd', cmd: data }))
    }
  })

  // Handle resize
  term.onResize(({ cols, rows }) => {
    if (ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'resize', cols, rows }))
    }
  })

  // Fit on window resize
  const resizeObserver = new ResizeObserver(() => fitAddon.fit())
  resizeObserver.observe(container)
  xtermResizeObserver = resizeObserver
}

let xtermResizeObserver: ResizeObserver | null = null

function cleanupXterm() {
  // Cancel any pending scheduled init so a queued initXterm cannot
  // fire after we've already torn down the previous instance.
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
}

function logoutSsh() {
  cleanupXterm()
  authenticated.value = false
  sshPassword.value = ''
  sshSessionToken = ''
  sshError.value = null
  connection.value = 'idle'
  connectionMessage.value = ''
}

// Reconnect re-runs the xterm boot path. The current session token is
// single-use and may have expired (60 s TTL on the backend), so we
// fall back to the login form when no usable token survives. The
// authenticated user re-enters credentials only in that case.
async function reconnectSsh() {
  if (!sshSessionToken) {
    // Token consumed — return to the login form so the user can re-auth.
    logoutSsh()
    return
  }
  cleanupXterm()
  await nextTick()
  scheduleInit(50)
}

// Seed the welcome banner on first open. Reseeds each time the dialog
// opens so users don't keep accumulating banner text across sessions.
watch(
  () => props.visible,
  (v) => {
    if (v) {
      authenticated.value = false
      sshPassword.value = ''
      sshError.value = null
      activeTab.value = 'terminal'
    } else {
      cleanupXterm()
    }
  },
)

// Cleanup on unmount to prevent ResizeObserver + WebSocket leaks
onBeforeUnmount(() => {
  cleanupXterm()
})

// Live theme follow — when the user flips dark mode, picks a different
// theme variant, or changes the accent color, push the new palette into
// the running xterm instance without a reconnect. xterm.js accepts a
// fresh theme object via the `options.theme` setter.
watch(
  () => [theme.prefs.darkMode, theme.prefs.themeVariant, theme.prefs.accentColor],
  () => {
    if (xtermInstance) {
      try {
        xtermInstance.options.theme = terminalPalette()
      } catch {
        // older xterm versions exposed `setOption` instead; ignore if
        // both paths fail so a single SDK bump doesn't crash the panel.
      }
    }
  },
)

// Add a keyboard shortcut to open the terminal — Ctrl/Cmd + ` matches
// VS Code's convention. Listener lives on window so it works from any
// settings page. Cleaned up in onBeforeUnmount above (window listener is
// idempotent — calling addEventListener with the same handler is safe).
function onWindowKey(e: KeyboardEvent) {
  if ((e.ctrlKey || e.metaKey) && e.key === '`') {
    e.preventDefault()
    emit('update:visible', !props.visible)
  }
}
if (typeof window !== 'undefined') {
  window.addEventListener('keydown', onWindowKey)
}
onBeforeUnmount(() => {
  if (typeof window !== 'undefined') {
    window.removeEventListener('keydown', onWindowKey)
  }
})

// Re-fit xterm when switching back to the terminal tab
watch(
  () => activeTab.value,
  (tab) => {
    if (tab === 'terminal' && xtermInstance && xtermFitAddon) {
      nextTick(() => {
        try { xtermFitAddon.fit() } catch {}
      })
    }
  },
)

// Click-anywhere inside the dialog refocuses the input so the terminal
// feels continuous — matches how real terminal emulators behave.
function focusInput() {
  inputRef.value?.focus()
}
</script>

<template>
  <Dialog
    v-model:visible="dialogVisible"
    modal
    :style="{
      width: '52rem',
      height: '34rem',
      border: '1px solid var(--cn-border)',
      borderRadius: '14px',
      overflow: 'hidden',
      boxShadow: '0 24px 64px -16px rgb(0 0 0 / 0.65)',
    }"
    :dismissable-mask="true"
    :show-header="false"
    content-class="!p-0 !h-full"
  >
    <div
      class="cn-terminal-shell flex h-full flex-col overflow-hidden rounded-[inherit]"
      @click="focusInput"
    >
      <!-- Custom header with tab strip (Terminal / Logs) + connection
           indicator + reconnect/close. Tabs and accent borders use CSS
           variables so the chrome tracks the active theme variant. -->
      <div class="cn-terminal-header flex items-center justify-between px-4">
        <div class="flex items-center gap-4">
          <button
            type="button"
            :class="[
              'inline-flex items-center gap-2 border-b-2 py-2 text-xs transition-colors',
              activeTab === 'terminal' ? 'cn-tab-active' : 'cn-tab-idle',
            ]"
            @click.stop="activeTab = 'terminal'"
          >
            <AppIcon name="Terminal" :size="13" />
            <span>{{ t('topbar.terminal') }}</span>
          </button>
          <button
            type="button"
            :class="[
              'inline-flex items-center gap-2 border-b-2 py-2 text-xs transition-colors',
              activeTab === 'logs' ? 'cn-tab-active' : 'cn-tab-idle',
            ]"
            @click.stop="activeTab = 'logs'"
          >
            <AppIcon name="FileText" :size="13" />
            <span>{{ t('terminal.logs') }}</span>
          </button>
        </div>
        <div class="flex items-center gap-2">
          <!-- Connection state dot. Color tracks status; tooltip-style
               text appears next to it only on closed/error. -->
          <div
            v-if="authenticated && activeTab === 'terminal'"
            class="flex items-center gap-1.5 text-[11px]"
          >
            <span
              class="inline-block h-2 w-2 rounded-full"
              :class="{
                'bg-emerald-500': connection === 'open',
                'bg-amber-500 animate-pulse': connection === 'connecting',
                'bg-rose-500': connection === 'error' || connection === 'closed',
                'bg-slate-500': connection === 'idle',
              }"
            />
            <span v-if="connection === 'closed' || connection === 'error'" class="text-rose-400">
              {{ connectionMessage }}
            </span>
          </div>
          <button
            v-if="authenticated && activeTab === 'terminal' && (connection === 'closed' || connection === 'error')"
            type="button"
            class="inline-flex items-center gap-1 rounded-md px-2 py-1 text-[11px] hover:bg-white/10"
            :style="{ color: 'var(--cn-accent)' }"
            :title="t('terminal.reconnect')"
            @click.stop="reconnectSsh"
          >
            <AppIcon name="RotateCcw" :size="12" />
            {{ t('terminal.reconnect') }}
          </button>
          <button
            type="button"
            class="flex h-6 w-6 items-center justify-center rounded-md text-slate-400 hover:bg-white/10 hover:text-slate-100"
            :aria-label="t('common.close')"
            @click.stop="dialogVisible = false"
          >
            <AppIcon name="X" :size="14" />
          </button>
        </div>
      </div>

      <!-- SSH login gate. Shell credentials are often distinct from
           the dashboard login, so we always start at the login form
           and only show the shell after the user authenticates. -->
      <div
        v-show="!authenticated && activeTab === 'terminal'"
        class="flex flex-1 items-center justify-center p-6"
        @click.stop
      >
        <form
          class="flex w-full max-w-sm flex-col gap-4"
          @submit.prevent="submitSsh"
        >
          <div class="mb-2 text-center">
            <div class="mx-auto mb-3 flex h-12 w-12 items-center justify-center rounded-full bg-slate-800 text-blue-400">
              <AppIcon name="LogIn" :size="22" />
            </div>
            <h3 class="text-base font-semibold text-slate-100">{{ t('terminal.login_title') }}</h3>
            <p class="mt-1 text-xs text-slate-500">{{ t('terminal.login_desc') }}</p>
          </div>

          <div>
            <label class="mb-1 block text-xs font-medium text-slate-400">
              {{ t('terminal.ssh_username') }}
            </label>
            <InputText
              v-model="sshUsername"
              class="w-full !bg-slate-800 !text-slate-100"
              autocomplete="off"
              autofocus
            />
          </div>

          <div>
            <label class="mb-1 block text-xs font-medium text-slate-400">
              {{ t('terminal.ssh_password') }}
            </label>
            <div class="relative">
              <InputText
                v-model="sshPassword"
                :type="sshPasswordVisible ? 'text' : 'password'"
                class="w-full !bg-slate-800 !text-slate-100 pr-9"
                autocomplete="off"
              />
              <button
                type="button"
                class="absolute right-2 top-1/2 -translate-y-1/2 text-slate-500 hover:text-slate-200"
                @click="sshPasswordVisible = !sshPasswordVisible"
              >
                <AppIcon :name="sshPasswordVisible ? 'EyeOff' : 'Eye'" :size="14" />
              </button>
            </div>
          </div>

          <div>
            <label class="mb-1 block text-xs font-medium text-slate-400">
              {{ t('terminal.ssh_port') }}
            </label>
            <InputNumber
              v-model="sshPort"
              :min="1"
              :max="65535"
              :useGrouping="false"
              class="w-full"
              inputClass="!bg-slate-800 !text-slate-100"
            />
          </div>

          <p v-if="sshError" class="text-center text-xs text-red-400">{{ sshError }}</p>

          <Button
            type="submit"
            :label="sshSubmitting ? t('terminal.ssh_connecting') : t('terminal.ssh_connect')"
            :disabled="!canSubmitSsh"
            class="w-full"
          />
        </form>
      </div>

      <!-- Authenticated shell (Terminal tab) — use v-show to preserve
           xterm.js DOM when switching to Logs tab and back -->
      <div v-show="authenticated && activeTab === 'terminal'" class="flex flex-1 flex-col overflow-hidden">
          <div
            ref="xtermRef"
            class="flex-1 bg-[#0f172a]"
          />
          <div class="flex items-center justify-end border-t border-slate-800 bg-[#0f1420] px-4 py-1">
            <button
              type="button"
              class="shrink-0 text-[10px] uppercase tracking-wide text-slate-500 hover:text-slate-300"
              @click="logoutSsh"
            >
              {{ t('terminal.ssh_logout') }}
            </button>
          </div>
      </div>

      <!-- Logs tab placeholder — stubbed until the journald/docker
           log streaming endpoint ships. -->
      <div v-show="activeTab === 'logs'" class="flex flex-1 flex-col items-center justify-center gap-3 p-6 text-center">
        <AppIcon name="FileText" :size="32" class="text-slate-600" />
        <p class="text-sm text-slate-400">{{ t('terminal.logs_placeholder') }}</p>
        <p class="text-xs text-slate-600">{{ t('terminal.logs_hint') }}</p>
      </div>
    </div>
  </Dialog>
</template>

<style scoped>
/* Terminal chrome — pulls from the global theme variables so a Longhorn
 * pane reads as one continuous surface with the rest of the app. We pin
 * the body to a dark mode regardless of the page theme because xterm
 * itself only ships a dark/light render path, and a light terminal on
 * a dark wallpaper is jarring. */
.cn-terminal-shell {
  background: var(--cn-surface, #0b0f1a);
  color: var(--cn-text, #e2e8f0);
}
.cn-terminal-header {
  background: color-mix(in srgb, var(--cn-surface, #0f1420) 92%, transparent);
  border-bottom: 1px solid var(--cn-border, rgb(30 41 59));
}
.cn-tab-active {
  border-color: var(--cn-accent, #3b82f6);
  color: var(--cn-text, #f1f5f9);
}
.cn-tab-idle {
  border-color: transparent;
  color: var(--cn-text-muted, #64748b);
}
.cn-tab-idle:hover {
  color: var(--cn-text, #cbd5e1);
}
</style>
