<script setup lang="ts">
// AppIcon — renders an icon from the user's chosen icon pack.
//
// Usage:
//   <AppIcon name="Settings" :size="20" />
//   <AppIcon name="ArrowDown" :size="16" class="text-blue-500" />
//
// The `name` prop uses Lucide naming (PascalCase). The component
// resolves the equivalent Phosphor icon automatically via the registry.

import { computed } from 'vue'
import { useIconPackStore } from '@/stores/iconPack'
import { lucideIcons, phosphorIcons } from './iconRegistry'

const props = withDefaults(
  defineProps<{
    name: string
    size?: number | string
    weight?: 'thin' | 'light' | 'regular' | 'bold' | 'fill' | 'duotone'
  }>(),
  { size: 20, weight: 'regular' },
)

const iconPack = useIconPackStore()

const iconComponent = computed(() => {
  if (iconPack.pack === 'phosphor') {
    return phosphorIcons[props.name] ?? lucideIcons[props.name]
  }
  return lucideIcons[props.name]
})

const iconProps = computed(() => {
  if (iconPack.pack === 'phosphor') {
    return { size: props.size, weight: props.weight }
  }
  return { size: props.size }
})
</script>

<template>
  <component :is="iconComponent" v-if="iconComponent" v-bind="iconProps" />
</template>
