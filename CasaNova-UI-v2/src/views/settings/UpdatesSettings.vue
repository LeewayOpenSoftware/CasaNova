<script setup lang="ts">
// Updates — the one place for everything upgradeable at the system level:
// CasaNova itself (self-update engine) and the underlying Linux distro (apt).
// Per-app updates live under Applications; this page is the OS/platform layer.

import { useI18n } from 'vue-i18n'
import SettingsPage from '@/components/settings/SettingsPage.vue'
import CasaNovaUpdateCard from '@/components/settings/CasaNovaUpdateCard.vue'
import AptUpdateCard from '@/components/settings/AptUpdateCard.vue'

const { t } = useI18n()
</script>

<template>
  <SettingsPage icon="RefreshCcw" :title="t('settings.updates.title')" :description="t('settings.updates.description')">
    <CasaNovaUpdateCard />
    <AptUpdateCard />
  </SettingsPage>
</template>
