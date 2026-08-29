<script setup lang="ts">
// Root component. Renders the router outlet plus the global Toast container
// that any descendant can push messages into via useToast().
import Toast from 'primevue/toast'
import { useThemeStore } from '@/stores/theme'

// Eagerly initialize the theme store so custom accent color, dark mode,
// and background preferences are applied on every page load — not just
// when a settings component happens to mount.
useThemeStore()
</script>

<template>
  <div class="min-h-screen">
    <router-view />
    <!-- Transient feedback lives top-right, under the top bar's notification
         bell — reads as a notification area rather than a faint corner popup. -->
    <Toast position="top-right" />
  </div>
</template>
