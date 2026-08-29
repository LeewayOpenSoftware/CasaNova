<script setup lang="ts">
// PortGlyph — tiny stylized illustration of a physical port.
//
// Pure CSS/div boxes (no SVG assets) so the mark renders at any size
// and inherits theme colors automatically. Each port kind has a
// recognizable silhouette.

import { computed } from 'vue'
import type { PortKind } from '@/api/hardware'

const props = defineProps<{
  kind: PortKind
  connected?: boolean
}>()

const dotClass = computed(() =>
  props.connected
    ? 'bg-emerald-500 shadow-[0_0_6px_rgba(16,185,129,0.6)]'
    : 'bg-[var(--cn-border)]',
)
</script>

<template>
  <div class="flex flex-col items-center gap-1.5">
    <!-- Ethernet: dark rectangle with 8 amber contact strips (RJ45) -->
    <div
      v-if="kind === 'ethernet'"
      class="relative h-8 w-12 rounded-md border border-slate-700 bg-slate-900"
    >
      <div class="absolute inset-x-1.5 top-1.5 flex h-3 gap-px">
        <div v-for="n in 8" :key="n" class="flex-1 bg-amber-500/70" />
      </div>
    </div>

    <!-- USB-A (3.x or 2.0): rectangle with colored bar. Blue=3.x, White=2.0 -->
    <div
      v-else-if="kind === 'usb3' || kind === 'usb2'"
      class="relative h-8 w-12 rounded-md border border-slate-700 bg-slate-900"
    >
      <div
        class="absolute inset-x-1.5 top-2 h-4 rounded-sm"
        :class="kind === 'usb3' ? 'bg-blue-500/70' : 'bg-slate-400/70'"
      >
        <div class="mt-2 h-1 rounded-sm bg-slate-900" />
      </div>
    </div>

    <!-- USB-C: small oval -->
    <div
      v-else-if="kind === 'usbc'"
      class="relative flex h-8 w-12 items-center justify-center rounded-md border border-slate-700 bg-slate-900"
    >
      <div class="h-3 w-6 rounded-full border border-slate-500 bg-slate-700" />
    </div>

    <!-- HDMI: wider trapezoid -->
    <div
      v-else-if="kind === 'hdmi'"
      class="relative h-8 w-12"
    >
      <div
        class="h-full w-full rounded-md border border-slate-700 bg-slate-900"
        style="clip-path: polygon(6% 0, 94% 0, 100% 40%, 100% 100%, 0 100%, 0 40%);"
      >
        <div class="mx-auto mt-3 h-2 w-8 rounded-sm bg-slate-500/60" />
      </div>
    </div>

    <!-- VGA: D-Sub trapezoid with screw holes -->
    <div
      v-else-if="kind === 'vga'"
      class="relative h-8 w-14"
    >
      <div
        class="h-full w-full rounded-md border border-slate-700 bg-blue-900/80"
        style="clip-path: polygon(8% 0, 92% 0, 100% 30%, 100% 100%, 0 100%, 0 30%);"
      >
        <div class="mx-auto mt-2.5 flex h-2 w-10 gap-px">
          <div v-for="n in 5" :key="n" class="flex-1 rounded-sm bg-amber-400/60" />
        </div>
      </div>
    </div>

    <!-- DisplayPort: stepped corner -->
    <div
      v-else-if="kind === 'displayport'"
      class="relative h-8 w-12"
    >
      <div
        class="h-full w-full rounded-md border border-slate-700 bg-slate-900"
        style="clip-path: polygon(0 0, 85% 0, 100% 30%, 100% 100%, 0 100%);"
      >
        <div class="mx-auto mt-3 h-2 w-7 rounded-sm bg-slate-500/60" />
      </div>
    </div>

    <!-- Audio: 3.5mm jack circle -->
    <div
      v-else-if="kind === 'audio'"
      class="flex h-8 w-8 items-center justify-center"
    >
      <div class="h-6 w-6 rounded-full border-2 border-slate-700 bg-slate-900">
        <div class="mx-auto mt-1.5 h-2 w-2 rounded-full bg-emerald-500/60" />
      </div>
    </div>

    <!-- PS/2: round DIN connector -->
    <div
      v-else-if="kind === 'ps2'"
      class="flex h-8 w-8 items-center justify-center"
    >
      <div class="h-7 w-7 rounded-full border-2 border-slate-700 bg-purple-900/60">
        <div class="mx-auto mt-1 flex h-3 w-3 flex-wrap gap-0.5">
          <div v-for="n in 6" :key="n" class="h-1 w-1 rounded-full bg-amber-400/60" />
        </div>
      </div>
    </div>

    <!-- Fallback: generic rectangle -->
    <div
      v-else
      class="h-8 w-12 rounded-md border border-slate-700 bg-slate-900"
    />

    <!-- Live link dot -->
    <span
      class="h-1.5 w-1.5 rounded-full transition-colors"
      :class="dotClass"
    />
  </div>
</template>
