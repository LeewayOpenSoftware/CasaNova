<script setup lang="ts">
// MorphIcon — an icon that ANIMATES between two states.
//
// Usage:
//   <MorphIcon :name="expanded ? 'ChevronDown' : 'ChevronRight'" :size="16" />
//   <MorphIcon :name="copied ? 'Check' : 'Copy'" :size="13" label="Copy link" />
//
// WHEN TO USE IT, AND WHEN NOT TO
//
// Only where the SAME element changes because its STATE changed: expanded ↔
// collapsed, copied ↔ not copied, editing ↔ viewing, hidden files shown ↔ not.
// The motion is the message — it says "this thing you are looking at just
// changed".
//
// NOT for choosing between two different things. `disk.isUSB ? 'Usb' :
// 'HardDrive'` picks an icon for a device; nothing changed, and animating it
// would claim a transition that never happened. Those keep using AppIcon.
//
// WHY THIS IS SEPARATE FROM AppIcon
//
// AppIcon is the icon system: every icon in the product goes through it, and it
// switches between the Lucide and Phosphor packs the user chose. This component
// is a leaf that sits beside it, so if morphicons is ever abandoned or changes
// its API, deleting this file and reverting a handful of call sites is the whole
// migration — instead of unpicking the icon system.
//
// PHOSPHOR FALLS BACK, IT DOES NOT BREAK
//
// Morphing interpolates stroke geometry on a 24×24 grid. Phosphor is a
// different grid with fill and duotone weights, so its icons are not
// interpolable this way. When the user has chosen Phosphor, this renders a
// plain AppIcon: the icon is still correct and still changes, it just does not
// animate. A pack choice must never cost the user a working control.

import { computed } from 'vue'
import { MorphIcon as Morph } from 'morphicons/vue'
import { isMorphable, morphableIcons } from './morphRegistry'
import { useIconPackStore } from '@/stores/iconPack'
import AppIcon from './AppIcon.vue'

const props = withDefaults(
  defineProps<{
    /** Lucide icon name in PascalCase, the same vocabulary AppIcon uses. */
    name: string
    size?: number | string
    /** Accessible label. Sets role="img" and a <title>; omit for decoration. */
    label?: string
    /** Animation character. "snappy" suits controls; "smooth" suits content. */
    spring?: 'smooth' | 'snappy' | 'bouncy'
  }>(),
  { size: 20, spring: 'snappy' },
)

const iconPack = useIconPackStore()

// Geometry comes from the explicit registry, not from a namespace import of
// the whole icon set: that version cost 102 kB gzipped for eight toggles,
// measured in the build output. A name outside the registry is not an error
// - it simply renders as an ordinary icon.
const iconNode = computed(() =>
  isMorphable(props.name) ? morphableIcons[props.name] : undefined,
)

// Morph only when we can: the user is on Lucide AND this name resolves to
// geometry. Anything else renders the ordinary icon rather than nothing.
const canMorph = computed(() => iconPack.pack !== 'phosphor' && iconNode.value !== undefined)
</script>

<template>
  <Morph
    v-if="canMorph"
    :icon="iconNode"
    :size="size"
    :spring="spring"
    :label="label"
    color="currentColor"
    stroke-width="2"
    reduced-motion="user"
  />
  <!--
    reduced-motion="user" rather than the library's "never" default: an
    operator who has asked their system for less motion has asked this
    application too, and a NAS dashboard is not the place to override that.
  -->
  <AppIcon v-else :name="name" :size="size" />
</template>
