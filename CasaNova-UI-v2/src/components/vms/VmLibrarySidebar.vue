<script setup lang="ts">
// The installation media and disk images a VM is built from.
//
// Both already existed only inside the create-VM wizard: you could pick an ISO
// while making a VM and nowhere else, and disks could not be made at all —
// the storage half of the virt API was read-only, so there was no path from
// "I want a VM" to "it has something to install onto" that did not go through
// a shell.
//
// They live in a sidebar rather than a page because they are a library you
// consult while looking at the VMs, not a destination of their own.

import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import { safeCall } from '@/api/safeCall'
import * as virtApi from '@/api/virt'

const { t } = useI18n()

const isos = ref<virtApi.IsoEntry[]>([])
const pools = ref<virtApi.StoragePool[]>([])
const volumes = ref<virtApi.StorageVolume[]>([])
const pool = ref('')
const busy = ref('')
const error = ref('')

// ── ISO ──────────────────────────────────────────────────────────────
const isoUrl = ref('')
const isoName = computed(() => {
  // The backend wants a filename; derive it from the URL rather than asking
  // for the same thing twice. A URL with a query string still ends in .iso
  // for every mirror we have seen, so the guess is worth making.
  try {
    const last = new URL(isoUrl.value).pathname.split('/').filter(Boolean).pop() ?? ''
    return last.toLowerCase().endsWith('.iso') ? last : ''
  } catch {
    return ''
  }
})

// ── disks ────────────────────────────────────────────────────────────
const diskName = ref('')
const diskGiB = ref(20)

function fmtBytes(n: number): string {
  if (!n) return '0 B'
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), u.length - 1)
  return `${(n / 1024 ** i).toFixed(i === 0 ? 0 : 1)} ${u[i]}`
}

async function loadISOs() {
  const r = await safeCall(() => virtApi.listISOs())
  if (r.data) isos.value = r.data
}

async function loadPools() {
  const r = await safeCall(() => virtApi.listPools())
  if (!r.data) return
  // Only a running pool can hold a new volume, and offering an inactive one
  // would fail at create time with a message about the pool rather than the
  // disk. Ranked so the default pool is preselected when it is available.
  pools.value = r.data.filter((p) => p.state === 'running')
  if (!pool.value && pools.value.length) {
    pool.value = (pools.value.find((p) => p.name === 'default') ?? pools.value[0]).name
  }
  await loadVolumes()
}

async function loadVolumes() {
  if (!pool.value) return
  const r = await safeCall(() => virtApi.listVolumes(pool.value))
  volumes.value = r.data ?? []
}

async function addISO() {
  if (!isoUrl.value || !isoName.value || busy.value) return
  busy.value = 'iso'
  error.value = ''
  const r = await safeCall(() => virtApi.downloadISO(isoUrl.value, isoName.value))
  busy.value = ''
  if (r.error) {
    error.value = r.error
    return
  }
  isoUrl.value = ''
  // The download runs in the background server-side, so the new entry does not
  // exist yet. Say so instead of refreshing into an unchanged list, which
  // reads as "nothing happened".
  error.value = t('vms.library.iso_queued')
  setTimeout(loadISOs, 4000)
}

async function removeISO(name: string) {
  if (!window.confirm(t('vms.library.iso_delete_confirm', { name }))) return
  busy.value = name
  error.value = ''
  const r = await safeCall(() => virtApi.deleteISO(name))
  busy.value = ''
  if (r.error) {
    error.value = r.error
    return
  }
  await loadISOs()
}

async function addDisk() {
  if (!diskName.value.trim() || !pool.value || busy.value) return
  busy.value = 'disk'
  error.value = ''
  const bytes = Math.round(diskGiB.value * 1024 ** 3)
  const r = await safeCall(() => virtApi.createVolume(pool.value, diskName.value.trim(), bytes))
  busy.value = ''
  if (r.error) {
    error.value = r.error
    return
  }
  diskName.value = ''
  await loadVolumes()
}

async function removeDisk(name: string) {
  if (!window.confirm(t('vms.library.disk_delete_confirm', { name }))) return
  busy.value = name
  error.value = ''
  const r = await safeCall(() => virtApi.deleteVolume(pool.value, name))
  busy.value = ''
  if (r.error) {
    error.value = r.error
    return
  }
  await loadVolumes()
}

onMounted(() => {
  loadISOs()
  loadPools()
})

defineExpose({ reload: () => { loadISOs(); loadVolumes() } })
</script>

<template>
  <aside class="flex w-full shrink-0 flex-col gap-4 lg:w-72">
    <!-- Installation media -->
    <section class="cn-widget flex flex-col gap-3 p-4">
      <header class="flex items-center gap-2">
        <AppIcon name="Archive" :size="16" class="text-[var(--cn-text-muted)]" />
        <h2 class="text-sm font-semibold text-[var(--cn-text)]">{{ t('vms.library.isos') }}</h2>
        <span class="ml-auto text-xs text-[var(--cn-text-muted)]">{{ isos.length }}</span>
      </header>

      <ul v-if="isos.length" class="flex flex-col gap-1">
        <li
          v-for="iso in isos"
          :key="iso.name"
          class="group flex items-center gap-2 rounded-md px-2 py-1.5 hover:bg-[var(--cn-surface-hover)]"
        >
          <div class="min-w-0 flex-1">
            <div class="truncate text-xs text-[var(--cn-text)]" :title="iso.name">{{ iso.name }}</div>
            <div class="text-[10px] text-[var(--cn-text-muted)]">{{ fmtBytes(iso.size_bytes) }}</div>
          </div>
          <button
            class="shrink-0 rounded p-1 text-[var(--cn-text-muted)] opacity-0 transition group-hover:opacity-100 hover:text-red-400"
            :title="t('vms.library.delete')"
            :disabled="busy === iso.name"
            @click="removeISO(iso.name)"
          >
            <AppIcon name="Trash2" :size="13" />
          </button>
        </li>
      </ul>
      <p v-else class="text-xs text-[var(--cn-text-muted)]">{{ t('vms.library.no_isos') }}</p>

      <div class="flex flex-col gap-2 border-t border-[var(--cn-border)] pt-3">
        <InputText
          v-model="isoUrl"
          class="w-full text-xs"
          :placeholder="t('vms.library.iso_url_placeholder')"
        />
        <Button
          size="small"
          class="w-full"
          :disabled="!isoName || busy === 'iso'"
          :loading="busy === 'iso'"
          :label="isoName ? t('vms.library.iso_add_named', { name: isoName }) : t('vms.library.iso_add')"
          @click="addISO"
        />
      </div>
    </section>

    <!-- Disk images -->
    <section class="cn-widget flex flex-col gap-3 p-4">
      <header class="flex items-center gap-2">
        <AppIcon name="HardDrive" :size="16" class="text-[var(--cn-text-muted)]" />
        <h2 class="text-sm font-semibold text-[var(--cn-text)]">{{ t('vms.library.disks') }}</h2>
        <span class="ml-auto text-xs text-[var(--cn-text-muted)]">{{ volumes.length }}</span>
      </header>

      <select
        v-if="pools.length > 1"
        v-model="pool"
        class="w-full rounded-md border border-[var(--cn-border)] bg-[var(--cn-surface)] px-2 py-1 text-xs text-[var(--cn-text)]"
        @change="loadVolumes"
      >
        <option v-for="p in pools" :key="p.name" :value="p.name">{{ p.name }}</option>
      </select>

      <ul v-if="volumes.length" class="flex flex-col gap-1">
        <li
          v-for="v in volumes"
          :key="v.key || v.name"
          class="group flex items-center gap-2 rounded-md px-2 py-1.5 hover:bg-[var(--cn-surface-hover)]"
        >
          <div class="min-w-0 flex-1">
            <div class="truncate text-xs text-[var(--cn-text)]" :title="v.path">{{ v.name }}</div>
            <!-- Both numbers, because a lazily-allocated disk's size on disk
                 and the size the guest sees differ by design. -->
            <div class="text-[10px] text-[var(--cn-text-muted)]">
              {{ fmtBytes(v.allocation) }} / {{ fmtBytes(v.capacity) }}
            </div>
          </div>
          <button
            class="shrink-0 rounded p-1 text-[var(--cn-text-muted)] opacity-0 transition group-hover:opacity-100 hover:text-red-400"
            :title="t('vms.library.delete')"
            :disabled="busy === v.name"
            @click="removeDisk(v.name)"
          >
            <AppIcon name="Trash2" :size="13" />
          </button>
        </li>
      </ul>
      <p v-else class="text-xs text-[var(--cn-text-muted)]">{{ t('vms.library.no_disks') }}</p>

      <div class="flex flex-col gap-2 border-t border-[var(--cn-border)] pt-3">
        <InputText
          v-model="diskName"
          class="w-full text-xs"
          :placeholder="t('vms.library.disk_name_placeholder')"
        />
        <div class="flex items-center gap-2">
          <input
            v-model.number="diskGiB"
            type="number"
            min="1"
            class="w-20 rounded-md border border-[var(--cn-border)] bg-[var(--cn-surface)] px-2 py-1 text-xs text-[var(--cn-text)]"
          />
          <span class="text-xs text-[var(--cn-text-muted)]">GiB</span>
          <Button
            size="small"
            class="ml-auto"
            :disabled="!diskName.trim() || !pool || busy === 'disk'"
            :loading="busy === 'disk'"
            :label="t('vms.library.disk_add')"
            @click="addDisk"
          />
        </div>
      </div>
    </section>

    <p v-if="error" class="px-1 text-xs text-[var(--cn-text-muted)]">{{ error }}</p>
  </aside>
</template>
