<script setup lang="ts">
/**
 * ShareLinkDialog — create a public HTTP share link for a file/folder.
 *
 * Two stages:
 *   1. Form — optional password, optional expiry (presets).
 *   2. Created — shows the URL with one-click copy. Also lists this
 *      path's other active links (revocable) so the user can clean up
 *      old links without leaving the dialog.
 */
import { ref, watch, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import Select from 'primevue/select'
import AppIcon from '@/components/ui/AppIcon.vue'
import MorphIcon from '@/components/ui/MorphIcon.vue'
import * as filesApi from '@/api/files'
import { safeCall } from '@/api/safeCall'

const props = defineProps<{
  visible: boolean
  path: string | null
}>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const { t } = useI18n()

const dialogVisible = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v),
})

const password = ref('')
const expiresHours = ref(0)
const busy = ref(false)
const error = ref('')
const created = ref<filesApi.ShareLinkView | null>(null)
const copied = ref(false)
const existing = ref<filesApi.ShareLinkView[]>([])

const expiryOptions = computed(() => [
  { value: 0, label: t('files.share_link.never') },
  { value: 1, label: t('files.share_link.one_hour') },
  { value: 24, label: t('files.share_link.one_day') },
  { value: 24 * 7, label: t('files.share_link.one_week') },
  { value: 24 * 30, label: t('files.share_link.one_month') },
])

watch(
  () => props.visible,
  async (open) => {
    if (!open) return
    password.value = ''
    expiresHours.value = 0
    created.value = null
    error.value = ''
    copied.value = false
    // Surface this path's existing links so the user doesn't pile up
    // duplicates without realizing.
    const res = await safeCall(() => filesApi.listShareLinks())
    existing.value = (res.data ?? []).filter((l) => l.path === props.path)
  },
)

async function create() {
  if (!props.path || busy.value) return
  busy.value = true
  error.value = ''
  const res = await safeCall(() =>
    filesApi.createShareLink(props.path!, {
      password: password.value || undefined,
      expiresHours: expiresHours.value || undefined,
    }),
  )
  busy.value = false
  if (res.error) {
    error.value = res.error
    return
  }
  created.value = res.data ?? null
}

async function copyUrl(url: string) {
  try {
    await navigator.clipboard.writeText(url)
    copied.value = true
    setTimeout(() => (copied.value = false), 1500)
  } catch {
    /* clipboard blocked */
  }
}

async function revoke(id: string) {
  await safeCall(() => filesApi.revokeShareLink(id))
  existing.value = existing.value.filter((l) => l.id !== id)
  if (created.value?.id === id) created.value = null
}
</script>

<template>
  <Dialog
    v-model:visible="dialogVisible"
    modal
    :style="{ width: '32rem' }"
    :header="t('files.share_link.title')"
    :dismissable-mask="!busy"
  >
    <div class="flex flex-col gap-4">
      <div class="truncate rounded-lg bg-[var(--cn-surface-hover)] px-3 py-2 font-mono text-xs text-[var(--cn-text-muted)]">
        {{ props.path }}
      </div>

      <template v-if="!created">
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('files.share_link.password_optional') }}
          </label>
          <InputText
            v-model="password"
            type="password"
            class="w-full"
            :placeholder="t('files.share_link.no_password')"
          />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">
            {{ t('files.share_link.expires') }}
          </label>
          <Select
            v-model="expiresHours"
            :options="expiryOptions"
            option-label="label"
            option-value="value"
            class="w-full"
          />
        </div>
        <p v-if="error" class="text-xs text-red-500">{{ error }}</p>
      </template>

      <template v-else>
        <div class="flex flex-col gap-2 rounded-lg border border-emerald-500/30 bg-emerald-500/5 p-3">
          <div class="flex items-center gap-2 text-xs text-emerald-500">
            <AppIcon name="CheckCircle2" :size="14" />
            {{ t('files.share_link.created') }}
          </div>
          <div class="flex items-center gap-2">
            <code class="min-w-0 flex-1 truncate rounded bg-[var(--cn-surface-hover)] px-2 py-1.5 font-mono text-xs text-[var(--cn-text)]">
              {{ created.url }}
            </code>
            <Button
              size="small"
              severity="secondary"
              @click="copyUrl(created.url)"
            >
              <template #icon>
                <MorphIcon :name="copied ? 'Check' : 'Copy'" :size="13" />
              </template>
            </Button>
          </div>
          <div class="flex gap-3 text-[10px] text-[var(--cn-text-muted)]">
            <span v-if="created.protected" class="inline-flex items-center gap-1">
              <AppIcon name="Lock" :size="10" /> {{ t('files.share_link.protected') }}
            </span>
            <span v-if="created.expiresAt" class="inline-flex items-center gap-1">
              <AppIcon name="Clock" :size="10" />
              {{ new Date(created.expiresAt * 1000).toLocaleString() }}
            </span>
          </div>
        </div>
      </template>

      <!-- Other active links for this path -->
      <div v-if="existing.length" class="flex flex-col gap-1">
        <div class="text-xs font-medium text-[var(--cn-text-muted)]">
          {{ t('files.share_link.existing') }}
        </div>
        <div
          v-for="l in existing"
          :key="l.id"
          class="flex items-center gap-2 rounded border border-[var(--cn-border)] px-2 py-1.5 text-xs"
        >
          <AppIcon name="Link2" :size="12" class="shrink-0 text-[var(--cn-text-muted)]" />
          <span class="min-w-0 flex-1 truncate font-mono text-[var(--cn-text-muted)]">{{ l.url }}</span>
          <span class="shrink-0 text-[10px] text-[var(--cn-text-muted)]">
            {{ l.downloads }}↓
          </span>
          <button
            type="button"
            class="shrink-0 text-red-500 hover:underline"
            @click="revoke(l.id)"
          >
            {{ t('files.share_link.revoke') }}
          </button>
        </div>
      </div>
    </div>

    <template #footer>
      <Button
        :label="t('common.close')"
        severity="secondary"
        :disabled="busy"
        @click="dialogVisible = false"
      />
      <Button
        v-if="!created"
        :label="t('files.share_link.create')"
        :loading="busy"
        :disabled="busy"
        @click="create"
      />
    </template>
  </Dialog>
</template>
