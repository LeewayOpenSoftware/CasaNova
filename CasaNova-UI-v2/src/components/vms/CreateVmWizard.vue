<script setup lang="ts">
// CreateVmWizard — a modal form to define a new VM: name, CPU, memory, a fresh
// qcow2 disk, firmware, an installer ISO (pick an existing one or add by URL),
// and optional cloud-init user-data. Maps to virtApi.createDomain.
import { ref, onMounted, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Dialog from 'primevue/dialog'
import Button from 'primevue/button'
import MorphIcon from '@/components/ui/MorphIcon.vue'
import { listISOs, downloadISO, createDomain, type IsoEntry } from '@/api/virt'

const visible = defineModel<boolean>('visible', { required: true })
const emit = defineEmits<{ (e: 'created', name: string): void }>()
const { t } = useI18n()

// form state
const name = ref('')
const vcpu = ref(2)
const memoryGiB = ref(2)
const diskGiB = ref(20)
const firmware = ref<'bios' | 'uefi'>('uefi')
const isoPath = ref<string>('')
const cloudInit = ref('')
const showCloudInit = ref(false)

const isos = ref<IsoEntry[]>([])
const submitting = ref(false)
const error = ref('')

// add-ISO-by-URL sub-form
const isoUrl = ref('')
const isoName = ref('')
const isoDownloading = ref(false)

const nameValid = computed(() => /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$/.test(name.value))
const canCreate = computed(() => nameValid.value && vcpu.value > 0 && memoryGiB.value > 0 && diskGiB.value > 0 && !submitting.value)

async function refreshIsos() {
  try { isos.value = await listISOs() } catch { isos.value = [] }
}
onMounted(refreshIsos)

function fmtSize(b: number): string {
  if (b >= 1024 ** 3) return (b / 1024 ** 3).toFixed(1) + ' GiB'
  if (b >= 1024 ** 2) return (b / 1024 ** 2).toFixed(0) + ' MiB'
  return b + ' B'
}

async function onDownloadIso() {
  if (!isoUrl.value.trim()) return
  isoDownloading.value = true
  error.value = ''
  try {
    const guessed = isoName.value.trim() || (isoUrl.value.split('/').pop() || 'image.iso')
    await downloadISO(isoUrl.value.trim(), guessed)
    isoUrl.value = ''
    isoName.value = ''
    // The download runs in the background; poll the list a few times so the
    // new ISO appears once it finishes.
    for (let i = 0; i < 6; i++) {
      await new Promise((r) => setTimeout(r, 5000))
      await refreshIsos()
    }
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    isoDownloading.value = false
  }
}

async function onCreate() {
  if (!canCreate.value) return
  submitting.value = true
  error.value = ''
  try {
    await createDomain({
      name: name.value.trim(),
      vcpu: vcpu.value,
      memoryMiB: Math.round(memoryGiB.value * 1024),
      diskGiB: diskGiB.value,
      firmware: firmware.value,
      isoPath: isoPath.value || undefined,
      cloudInitUserData: showCloudInit.value ? cloudInit.value : undefined,
    })
    emit('created', name.value.trim())
    visible.value = false
    // reset for next open
    name.value = ''; cloudInit.value = ''; isoPath.value = ''; showCloudInit.value = false
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    submitting.value = false
  }
}

const inputCls =
  'w-full rounded-lg border border-[var(--cn-border)] bg-[var(--cn-surface)] px-3 py-2 text-sm text-[var(--cn-text)] focus:border-blue-500 focus:outline-none'
</script>

<template>
  <Dialog v-model:visible="visible" modal :header="t('vms.create.title')" :style="{ width: '34rem', maxWidth: '94vw' }">
    <div class="flex flex-col gap-4">
      <!-- name -->
      <div>
        <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">{{ t('vms.create.name') }}</label>
        <input v-model="name" :class="inputCls" placeholder="ubuntu-server" />
        <p v-if="name && !nameValid" class="mt-1 text-xs text-red-500">{{ t('vms.create.name_invalid') }}</p>
      </div>

      <!-- cpu + memory + disk -->
      <div class="grid grid-cols-3 gap-3">
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">{{ t('vms.create.vcpu') }}</label>
          <input v-model.number="vcpu" type="number" min="1" max="64" :class="inputCls" />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">{{ t('vms.create.memory') }}</label>
          <input v-model.number="memoryGiB" type="number" min="0.5" step="0.5" :class="inputCls" />
        </div>
        <div>
          <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">{{ t('vms.create.disk') }}</label>
          <input v-model.number="diskGiB" type="number" min="1" :class="inputCls" />
        </div>
      </div>

      <!-- firmware -->
      <div>
        <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">{{ t('vms.create.firmware') }}</label>
        <div class="flex gap-2">
          <button
            v-for="fw in (['uefi', 'bios'] as const)" :key="fw" type="button"
            class="flex-1 rounded-lg border px-3 py-2 text-sm transition-colors"
            :class="firmware === fw ? 'border-blue-500 bg-blue-500/10 text-blue-500' : 'border-[var(--cn-border)] text-[var(--cn-text-muted)] hover:border-[var(--cn-text)]'"
            @click="firmware = fw"
          >{{ fw.toUpperCase() }}</button>
        </div>
      </div>

      <!-- ISO -->
      <div>
        <label class="mb-1 block text-xs font-medium text-[var(--cn-text-muted)]">{{ t('vms.create.iso') }}</label>
        <select v-model="isoPath" :class="inputCls">
          <option value="">{{ t('vms.create.iso_none') }}</option>
          <option v-for="iso in isos" :key="iso.path" :value="iso.path">{{ iso.name }} ({{ fmtSize(iso.size_bytes) }})</option>
        </select>
        <!-- add ISO by URL -->
        <div class="mt-2 flex items-end gap-2">
          <div class="flex-1">
            <input v-model="isoUrl" :class="inputCls" :placeholder="t('vms.create.iso_url')" />
          </div>
          <Button size="small" severity="secondary" :loading="isoDownloading" :disabled="!isoUrl.trim()" :label="t('vms.create.iso_add')" @click="onDownloadIso" />
        </div>
        <p v-if="isoDownloading" class="mt-1 text-xs text-[var(--cn-text-muted)]">{{ t('vms.create.iso_downloading') }}</p>
      </div>

      <!-- cloud-init -->
      <div>
        <button type="button" class="flex items-center gap-1.5 text-xs font-medium text-[var(--cn-text-muted)] hover:text-[var(--cn-text)]" @click="showCloudInit = !showCloudInit">
          <MorphIcon :name="showCloudInit ? 'ChevronDown' : 'ChevronRight'" :size="14" />
          {{ t('vms.create.cloud_init') }}
        </button>
        <div v-if="showCloudInit" class="mt-2">
          <textarea
            v-model="cloudInit" rows="6" spellcheck="false"
            :class="[inputCls, 'font-mono text-xs']"
            :placeholder="'#cloud-config\nusers:\n  - name: admin\n    sudo: ALL=(ALL) NOPASSWD:ALL\n    ssh_authorized_keys:\n      - ssh-ed25519 AAAA...'"
          />
          <p class="mt-1 text-xs text-[var(--cn-text-muted)]">{{ t('vms.create.cloud_init_hint') }}</p>
        </div>
      </div>

      <div v-if="error" class="rounded-lg border border-red-500/30 bg-red-500/10 p-2.5 text-xs text-red-500">{{ error }}</div>
    </div>

    <template #footer>
      <Button severity="secondary" :label="t('common.cancel')" @click="visible = false" />
      <Button :disabled="!canCreate" :loading="submitting" :label="t('vms.create.submit')" @click="onCreate" />
    </template>
  </Dialog>
</template>
