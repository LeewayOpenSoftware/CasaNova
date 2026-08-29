<script setup lang="ts">
// AppLayout — the authenticated shell. Wraps every page behind /login
// with the persistent TopBar plus the global NotificationSidebar, and
// renders the current route's component inside a scrollable <main>.
//
// Mounted once per session (router.beforeEach doesn't tear down the
// layout between sibling navigations), so this is the right place to
// kick off notification polling. The store's startPolling is idempotent
// so a re-mount after logout+login cycles cleanly.

import { onMounted, onBeforeUnmount } from 'vue'
import { useNotificationsStore } from '@/stores/notifications'
import { useUpdatesStore } from '@/stores/updates'
import TopBar from './TopBar.vue'
import NotificationSidebar from './NotificationSidebar.vue'
import ToastStack from './ToastStack.vue'

const notifications = useNotificationsStore()
const updates = useUpdatesStore()

onMounted(() => {
  notifications.startPolling()
  updates.startPolling()
})

onBeforeUnmount(() => {
  notifications.stopPolling()
  updates.stopPolling()
})
</script>

<template>
  <div class="flex h-screen flex-col">
    <TopBar />
    <main class="flex-1 overflow-auto">
      <router-view />
    </main>
    <NotificationSidebar />
    <ToastStack />
  </div>
</template>
