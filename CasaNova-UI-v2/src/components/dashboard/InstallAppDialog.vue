<script setup lang="ts">
// InstallAppDialog — three-mode install sheet matching the CasaOS
// "Instalar aplicación" flow.
//
// Tabs:
//   1. Manual — the full ContainerEntry-shaped form (image, tag,
//      title, icon URL, Web UI composite, network). Submitting kicks
//      off the install progress simulation.
//   2. External link — add a non-container tile that just links to
//      an existing web service (for apps hosted outside CasaNova).
//   3. Import — paste a Docker Compose file or a `docker run` CLI
//      one-liner. When the backend lands this parses and installs;
//      for the scaffold we just echo the import back as an install.
//
// All three modes funnel into the same installProgressStore action
// so the in-progress card and toast feedback are unified.

import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import Textarea from 'primevue/textarea'
import Select from 'primevue/select'
import AppIcon from '@/components/ui/AppIcon.vue'
import { useInstallProgressStore } from '@/stores/installProgress'
import { useContainersStore } from '@/stores/containers'
import { containersApi } from '@/api'
import type { DockerHubResult } from '@/api/containers'
import { safeCall } from '@/api/safeCall'

const props = defineProps<{
  visible: boolean
  prefillYaml?: string
}>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const { t } = useI18n()
const installs = useInstallProgressStore()
const containersStore = useContainersStore()

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

type Mode = 'manual' | 'link' | 'import' | 'dockerhub'
const mode = ref<Mode>('manual')

// ----- Docker Hub search -----
const dhQuery = ref('')
const dhResults = ref<DockerHubResult[]>([])
const dhLoading = ref(false)
const dhSearched = ref(false)
const dhError = ref('')

async function runDockerHubSearch() {
  const q = dhQuery.value.trim()
  if (!q) return
  dhLoading.value = true
  dhError.value = ''
  try {
    dhResults.value = await containersApi.searchDockerHub(q)
    dhSearched.value = true
  } catch (e) {
    dhError.value = e instanceof Error ? e.message : String(e)
    dhResults.value = []
  } finally {
    dhLoading.value = false
  }
}

// Picking a Docker Hub image drops you into the manual form pre-filled with the
// image + a sensible title, so you can set ports/volumes then install.
function pickDockerHubImage(r: DockerHubResult) {
  manual.value.dockerImage = r.name
  manual.value.imageTag = 'latest'
  const short = (r.name.split('/').pop() || r.name)
  manual.value.title = short.charAt(0).toUpperCase() + short.slice(1)
  mode.value = 'manual'
}

// Docker Hub's v4 search returns the pull count already rounded, as its own
// wording ("1B+", "50K+"). The local formatter it replaces took an exact
// number and produced a different approximation; keeping Hub's string means
// the dashboard says exactly what Hub says.

// When prefillYaml is provided (from the library's "Custom Install"),
// auto-switch to Import → Compose mode and populate the textarea.
watch(
  () => props.prefillYaml,
  (yaml) => {
    if (yaml) {
      mode.value = 'import'
      importMode.value = 'compose'
      importContent.value = yaml
    }
  },
)

// ----- Manual install form -----
const manual = ref({
  dockerImage: '',
  imageTag: 'latest',
  title: '',
  iconUrl: '',
  webUIProtocol: 'http' as 'http' | 'https',
  webUIHost: '',
  webUIPort: null as number | null,
  webUIPath: '/',
  network: 'bridge',
})

const manualValid = computed(() => {
  return manual.value.dockerImage.trim().length > 0 && manual.value.title.trim().length > 0
})

async function submitManual() {
  if (!manualValid.value) return
  // Capture all form values BEFORE resetting — resetForms() zeroes the
  // reactive refs, so any reads after it would see empty defaults.
  const snapshot = { ...manual.value }
  const name = snapshot.title || snapshot.dockerImage
  const iconUrl = snapshot.iconUrl || undefined
  // Close the dialog + begin the fake progress card first so the
  // user sees immediate feedback; the real backend call runs in the
  // background and the progress store's tick simulation fills in
  // until the backend's SSE stream replaces it.
  installs.beginInstall(name, iconUrl)
  dialogVisible.value = false
  resetForms()

  const result = await safeCall(
    () =>
      containersApi.installManual({
        image: snapshot.dockerImage,
        tag: snapshot.imageTag,
        title: name,
        iconUrl,
        webUI:
          snapshot.webUIPort !== null
            ? {
                protocol: snapshot.webUIProtocol,
                host: snapshot.webUIHost,
                port: snapshot.webUIPort,
                path: snapshot.webUIPath,
              }
            : undefined,
        network: snapshot.network,
      }),
  )
  if (result.error) {
    installs.pushToast('error', result.error)
    return
  }
  if (!result.data) return
  // Refresh the containers list so the new app shows up on the
  // dashboard grid + Containers settings page.
  setTimeout(() => containersStore.refresh(true), 6000)
}

// ----- External link form -----
const external = ref({
  url: 'http://',
  title: '',
  iconUrl: '',
})
const externalValid = computed(
  () =>
    external.value.url.trim().length > 7 &&
    external.value.title.trim().length > 0,
)
function submitExternal() {
  if (!externalValid.value) return
  installs.beginInstall(external.value.title, external.value.iconUrl || undefined)
  dialogVisible.value = false
  resetForms()
}

// ----- Import form -----
const importMode = ref<'compose' | 'cli'>('compose')
const importContent = ref('')
const importValid = computed(() => importContent.value.trim().length > 0)
async function submitImport() {
  if (!importValid.value) return
  const name = deriveNameFromImport(importContent.value) || 'imported app'
  const content = importContent.value
  const mode = importMode.value
  installs.beginInstall(name)
  dialogVisible.value = false
  resetForms()

  const result = await safeCall(
    () =>
      mode === 'compose'
        ? containersApi.installFromCompose(content)
        : containersApi.installFromCli(content),
  )
  if (result.error) {
    installs.pushToast('error', result.error)
    return
  }
  if (!result.data) return
  setTimeout(() => containersStore.refresh(true), 6000)
}

function deriveNameFromImport(text: string): string | null {
  // Compose: first service key under `services:`
  const svcMatch = text.match(/services:\s*\n\s+([a-z0-9_-]+):/i)
  if (svcMatch) return svcMatch[1]
  // CLI: `docker run ... --name foo`
  const nameMatch = text.match(/--name[\s=]+([a-z0-9_-]+)/i)
  if (nameMatch) return nameMatch[1]
  // CLI fallback: last image name
  const imgMatch = text.match(/([a-z0-9-]+\/[a-z0-9-]+|[a-z0-9-]+):[a-z0-9._-]+/i)
  if (imgMatch) return imgMatch[1]
  return null
}

function resetForms() {
  manual.value = {
    dockerImage: '',
    imageTag: 'latest',
    title: '',
    iconUrl: '',
    webUIProtocol: 'http',
    webUIHost: '',
    webUIPort: null,
    webUIPath: '/',
    network: 'bridge',
  }
  external.value = { url: 'http://', title: '', iconUrl: '' }
  importContent.value = ''
  importMode.value = 'compose'
  mode.value = 'manual'
}

function onCancel() {
  dialogVisible.value = false
  resetForms()
}
</script>

<template>
  <Dialog
    v-model:visible="dialogVisible"
    modal
    :style="{ width: '44rem' }"
    :dismissable-mask="true"
    :header="t('install.title')"
  >
    <!-- Top mode tabs -->
    <div class="mb-5 flex gap-1 border-b border-[var(--cn-border)]">
      <button
        type="button"
        :class="[
          'inline-flex items-center gap-2 border-b-2 px-4 py-2 text-sm font-medium transition-colors',
          mode === 'manual'
            ? 'border-blue-500 text-blue-500'
            : 'border-transparent text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]',
        ]"
        @click="mode = 'manual'"
      >
        <AppIcon name="Container" :size="14" />
        {{ t('install.manual') }}
      </button>
      <button
        type="button"
        :class="[
          'inline-flex items-center gap-2 border-b-2 px-4 py-2 text-sm font-medium transition-colors',
          mode === 'link'
            ? 'border-blue-500 text-blue-500'
            : 'border-transparent text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]',
        ]"
        @click="mode = 'link'"
      >
        <AppIcon name="ExternalLink" :size="14" />
        {{ t('install.external_link') }}
      </button>
      <button
        type="button"
        :class="[
          'inline-flex items-center gap-2 border-b-2 px-4 py-2 text-sm font-medium transition-colors',
          mode === 'import'
            ? 'border-blue-500 text-blue-500'
            : 'border-transparent text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]',
        ]"
        @click="mode = 'import'"
      >
        <AppIcon name="Upload" :size="14" />
        {{ t('install.import') }}
      </button>
      <button
        type="button"
        :class="[
          'inline-flex items-center gap-2 border-b-2 px-4 py-2 text-sm font-medium transition-colors',
          mode === 'dockerhub'
            ? 'border-blue-500 text-blue-500'
            : 'border-transparent text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]',
        ]"
        @click="mode = 'dockerhub'"
      >
        <AppIcon name="Search" :size="14" />
        {{ t('install.dockerhub') }}
      </button>
    </div>

    <!-- Docker Hub search -->
    <div v-if="mode === 'dockerhub'" class="flex flex-col gap-3">
      <div class="flex items-end gap-2">
        <div class="flex-1">
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">{{ t('install.dockerhub_search') }}</label>
          <input
            v-model="dhQuery"
            class="w-full rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface)] px-3 py-2 text-sm text-[var(--cn-text)] focus:border-blue-500 focus:outline-none"
            :placeholder="t('install.dockerhub_placeholder')"
            @keyup.enter="runDockerHubSearch"
          />
        </div>
        <Button size="small" :loading="dhLoading" :disabled="!dhQuery.trim()" :label="t('install.dockerhub_search_btn')" @click="runDockerHubSearch" />
      </div>
      <p v-if="dhError" class="text-xs text-red-500">{{ dhError }}</p>
      <div v-if="dhResults.length" class="flex max-h-[46vh] flex-col gap-1.5 overflow-y-auto">
        <button
          v-for="r in dhResults" :key="r.name" type="button"
          class="flex items-start gap-3 rounded-lg border border-[var(--cn-border)] p-2.5 text-left transition-colors hover:border-blue-500 hover:bg-[var(--cn-surface-hover)]"
          @click="pickDockerHubImage(r)"
        >
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-2">
              <span class="truncate font-mono text-sm font-medium text-[var(--cn-text)]">{{ r.name }}</span>
              <span v-if="r.official" class="shrink-0 rounded bg-blue-500/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-blue-500">{{ t('install.dockerhub_official') }}</span>
            </div>
            <p v-if="r.description" class="mt-0.5 truncate text-xs text-[var(--cn-text-muted)]">{{ r.description }}</p>
            <div class="mt-1 flex items-center gap-3 text-[11px] text-[var(--cn-text-muted)]">
              <span class="flex items-center gap-1"><AppIcon name="Star" :size="11" /> {{ r.stars.toLocaleString() }}</span>
              <span class="flex items-center gap-1"><AppIcon name="Download" :size="11" /> {{ r.pulls }}</span>
            </div>
          </div>
          <AppIcon name="ChevronRight" :size="16" class="mt-1 shrink-0 text-[var(--cn-text-muted)]" />
        </button>
      </div>
      <p v-else-if="dhSearched && !dhLoading" class="py-6 text-center text-sm text-[var(--cn-text-muted)]">{{ t('install.dockerhub_no_results') }}</p>
      <p v-else class="py-6 text-center text-xs text-[var(--cn-text-muted)]">{{ t('install.dockerhub_hint') }}</p>
    </div>

    <!-- Manual install form -->
    <div v-if="mode === 'manual'" class="flex flex-col gap-4">
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-[2fr_1fr]">
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
            {{ t('apps.docker_image') }} *
          </label>
          <div class="relative">
            <InputText
              v-model="manual.dockerImage"
              placeholder="e.g. deluan/navidrome"
              class="w-full font-mono text-xs"
            />
            <AppIcon
              v-if="manual.dockerImage"
              name="Check"
              :size="15"
              class="absolute right-3 top-1/2 -translate-y-1/2 text-emerald-500"
            />
          </div>
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
            {{ t('apps.tag') }}
          </label>
          <InputText v-model="manual.imageTag" class="w-full font-mono text-xs" />
        </div>
      </div>
      <div>
        <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
          {{ t('apps.title_label') }} *
        </label>
        <InputText
          v-model="manual.title"
          :placeholder="t('apps.title_placeholder')"
          class="w-full"
        />
      </div>
      <div>
        <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
          {{ t('apps.icon_url') }}
        </label>
        <div class="flex items-center gap-2">
          <div
            class="flex h-10 w-10 shrink-0 items-center justify-center overflow-hidden rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)]"
          >
            <img
              v-if="manual.iconUrl"
              :src="manual.iconUrl"
              :alt="manual.title"
              class="h-full w-full object-contain"
            />
            <AppIcon v-else name="Container" :size="18" class="text-[var(--cn-text-muted)]" />
          </div>
          <InputText
            v-model="manual.iconUrl"
            class="flex-1 font-mono text-xs"
            :placeholder="t('install.icon_placeholder')"
          />
        </div>
      </div>
      <div>
        <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
          {{ t('apps.web_ui') }}
        </label>
        <div class="grid grid-cols-[auto_1fr_auto_1fr] items-center gap-2">
          <Select
            v-model="manual.webUIProtocol"
            :options="[
              { value: 'http', label: 'http://' },
              { value: 'https', label: 'https://' },
            ]"
            option-label="label"
            option-value="value"
            class="w-28"
          />
          <InputText
            v-model="manual.webUIHost"
            :placeholder="t('apps.web_ui_host')"
            class="w-full"
          />
          <div class="flex items-center gap-1">
            <span class="text-[var(--cn-text-muted)]">:</span>
            <InputText
              :model-value="manual.webUIPort != null ? String(manual.webUIPort) : ''"
              type="number"
              :placeholder="t('install.ports_placeholder')"
              class="w-24"
              @update:model-value="manual.webUIPort = $event ? Number($event) : null"
            />
          </div>
          <InputText v-model="manual.webUIPath" class="w-full" placeholder="/" />
        </div>
      </div>
      <div>
        <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
          {{ t('apps.network') }}
        </label>
        <Select
          v-model="manual.network"
          editable
          :options="['bridge', 'host', 'none']"
          class="w-full sm:w-72"
        />
      </div>
    </div>

    <!-- External link form -->
    <div v-else-if="mode === 'link'" class="flex flex-col gap-4">
      <p class="text-xs text-[var(--cn-text-muted)]">
        {{ t('install.external_link_desc') }}
      </p>
      <div>
        <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
          {{ t('install.link_url') }} <span class="text-red-500">*</span>
        </label>
        <InputText v-model="external.url" class="w-full font-mono text-xs" />
      </div>
      <div>
        <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
          {{ t('apps.title_label') }} <span class="text-red-500">*</span>
        </label>
        <InputText
          v-model="external.title"
          :placeholder="t('install.link_title_placeholder')"
          class="w-full"
        />
      </div>
      <div>
        <label class="mb-1 block text-xs font-medium text-[var(--cn-text)]">
          {{ t('apps.icon_url') }}
        </label>
        <div class="flex items-center gap-2">
          <div
            class="flex h-10 w-10 shrink-0 items-center justify-center overflow-hidden rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface-hover)]"
          >
            <img
              v-if="external.iconUrl"
              :src="external.iconUrl"
              :alt="external.title"
              class="h-full w-full object-contain"
            />
            <AppIcon v-else name="ExternalLink" :size="18" class="text-[var(--cn-text-muted)]" />
          </div>
          <InputText
            v-model="external.iconUrl"
            class="flex-1 font-mono text-xs"
            :placeholder="t('install.icon_placeholder')"
          />
        </div>
      </div>
    </div>

    <!-- Import form -->
    <div v-else class="flex flex-col gap-3">
      <!-- Sub-tabs: Compose / CLI -->
      <div class="flex gap-1 border-b border-[var(--cn-border)]">
        <button
          type="button"
          :class="[
            'inline-flex items-center gap-2 border-b-2 px-4 py-2 text-sm font-medium transition-colors',
            importMode === 'compose'
              ? 'border-blue-500 text-blue-500'
              : 'border-transparent text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]',
          ]"
          @click="importMode = 'compose'"
        >
          <AppIcon name="FileCode" :size="13" />
          Docker Compose
        </button>
        <button
          type="button"
          :class="[
            'inline-flex items-center gap-2 border-b-2 px-4 py-2 text-sm font-medium transition-colors',
            importMode === 'cli'
              ? 'border-blue-500 text-blue-500'
              : 'border-transparent text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]',
          ]"
          @click="importMode = 'cli'"
        >
          <AppIcon name="Terminal" :size="13" />
          Docker CLI
        </button>
      </div>

      <Textarea
        v-model="importContent"
        :placeholder="
          importMode === 'compose'
            ? t('install.import_compose_placeholder')
            : t('install.import_cli_placeholder')
        "
        rows="12"
        class="w-full font-mono text-xs"
      />
      <p v-if="importMode === 'compose'" class="text-[11px] text-[var(--cn-text-muted)]">
        {{ t('install.import_compose_hint') }}
      </p>
    </div>

    <template #footer>
      <Button :label="t('common.cancel')" severity="secondary" @click="onCancel" />
      <Button
        v-if="mode === 'manual'"
        :label="t('install.install')"
        :disabled="!manualValid"
        @click="submitManual"
      />
      <Button
        v-else-if="mode === 'link'"
        :label="t('install.connect')"
        :disabled="!externalValid"
        @click="submitExternal"
      />
      <Button
        v-else
        :label="t('install.send')"
        :disabled="!importValid"
        @click="submitImport"
      />
    </template>
  </Dialog>
</template>
