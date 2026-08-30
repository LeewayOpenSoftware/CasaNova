<script setup lang="ts">
// AppLayout — the authenticated shell. Wraps every page behind /login
// with the persistent TopBar plus the global NotificationSidebar, and
// renders the current route's component inside a scrollable <main>.
//
// Mounted once per session (router.beforeEach doesn't tear down the
// layout between sibling navigations), so this is the right place to
// kick off notification polling. The store's startPolling is idempotent
// so a re-mount after logout+login cycles cleanly.

import { onMounted, onBeforeUnmount, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { useNotificationsStore } from '@/stores/notifications'
import { useUpdatesStore } from '@/stores/updates'
import TopBar from './TopBar.vue'
import NotificationSidebar from './NotificationSidebar.vue'
import ToastStack from './ToastStack.vue'

const notifications = useNotificationsStore()
const updates = useUpdatesStore()

// The scroll container for every routed view is THIS <main>, not the
// window — so vue-router's scrollBehavior never sees it, and navigating
// carried the previous page's scroll offset along. The visible symptom:
// click "Acerca de" at the bottom of the settings rail and the new page
// opens already scrolled to its far bottom. Reset on every path change;
// same-path changes (query/hash) are left alone so in-page anchors and
// filter params don't yank the viewport.
const route = useRoute()
const mainEl = ref<HTMLElement | null>(null)
watch(
  () => route.path,
  () => {
    mainEl.value?.scrollTo({ top: 0 })
  },
)

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
    <main ref="mainEl" class="flex-1 overflow-auto">
      <router-view />
    </main>
    <NotificationSidebar />
    <ToastStack />
  </div>
</template>
