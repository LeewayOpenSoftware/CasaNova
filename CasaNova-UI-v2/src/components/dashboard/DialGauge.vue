<script setup lang="ts">
// DialGauge — resource meter for CPU / RAM / disk with four visual
// variants the user can switch between:
//
//   - 'ring' (default): the CasaOS circular ring — single value arc
//     with the big number centered inside
//   - 'digital': LED-style big number over a horizontal segment bar
//     with left/right tick marks
//   - 'analog': classic speedometer needle on a 180° arc
//   - 'cockpit': Peugeot i-Cockpit-inspired neon arcs with glow,
//     tick lines, and a floating numeric readout
//
// All variants share the same props (value 0-100, label, subtext,
// color) so the parent widget can swap between them with a single
// ref change — see systemGaugeStyle in useUserPrefs.
//
// Hand-rolled SVG everywhere because the dials are simple, we control
// every pixel, and we stay dependency-free.

import { computed } from 'vue'

type GaugeVariant = 'ring' | 'digital' | 'analog' | 'cockpit'

const props = withDefaults(
  defineProps<{
    value: number
    label: string
    subtext?: string
    color?: string
    variant?: GaugeVariant
  }>(),
  {
    color: 'rgb(59 130 246)',
    subtext: '',
    variant: 'ring',
  },
)

const clamped = computed(() => Math.max(0, Math.min(100, props.value)))

// ----- Ring geometry (variant: ring) -----
const radius = 50
const circumference = 2 * Math.PI * radius
const ringOffset = computed(() => circumference * (1 - clamped.value / 100))

// ----- Analog needle (variant: analog) -----
// Needle rotates from -90° (0%) to +90° (100%). The SVG arc spans
// 180° and the center is at (60, 60).
const needleAngle = computed(() => -90 + (clamped.value / 100) * 180)

// ----- Cockpit / i-Cockpit-ish (variant: cockpit) -----
// Same 180° sweep but with a neon-style arc + glow filter baked in.
// The sweep starts at 150° (bottom-left) and ends at 30° (bottom-
// right), covering 240° total — wider than the analog variant.
const COCKPIT_START = 150
const COCKPIT_END = 30
const COCKPIT_SWEEP = 360 - (COCKPIT_END - COCKPIT_START + 360) % 360 // 240
const cockpitAngle = computed(
  () => (COCKPIT_START + (COCKPIT_SWEEP * clamped.value) / 100) % 360,
)
function polar(cx: number, cy: number, r: number, deg: number) {
  const rad = ((deg - 180) * Math.PI) / 180
  return { x: cx + r * Math.cos(rad), y: cy + r * Math.sin(rad) }
}
const cockpitArcPath = computed(() => {
  const start = polar(60, 60, 48, COCKPIT_START)
  const end = polar(60, 60, 48, cockpitAngle.value)
  const largeArc = clamped.value > 50 ? 1 : 0
  return `M ${start.x} ${start.y} A 48 48 0 ${largeArc} 1 ${end.x} ${end.y}`
})
const cockpitTrackPath = computed(() => {
  const start = polar(60, 60, 48, COCKPIT_START)
  const end = polar(60, 60, 48, COCKPIT_END)
  return `M ${start.x} ${start.y} A 48 48 0 1 1 ${end.x} ${end.y}`
})

// ----- Digital (variant: digital) -----
// Bar is 20 cells; count lit cells from the percentage.
const digitalCells = computed(() => {
  const filled = Math.round((clamped.value / 100) * 20)
  return Array.from({ length: 20 }, (_, i) => i < filled)
})
</script>

<template>
  <div class="flex flex-col items-center">
    <!-- Variant 1: RING (default) — CasaOS-style circular meter. The
         overlay is absolutely positioned inside a relative wrapper so
         the label is mathematically centered on the ring no matter
         what the SVG stroke width is. -->
    <div v-if="variant === 'ring'" class="relative h-24 w-24">
      <svg viewBox="0 0 120 120" class="h-full w-full -rotate-90">
        <circle cx="60" cy="60" :r="radius" fill="none" stroke="var(--cn-border)" stroke-width="8" />
        <circle
          cx="60"
          cy="60"
          :r="radius"
          fill="none"
          :stroke="color"
          stroke-width="8"
          stroke-linecap="round"
          :stroke-dasharray="circumference"
          :stroke-dashoffset="ringOffset"
          class="transition-[stroke-dashoffset] duration-700 ease-out"
        />
      </svg>
      <div class="absolute inset-0 flex items-center justify-center">
        <span class="text-2xl font-semibold tabular-nums text-[var(--cn-text)]">
          {{ Math.round(value) }}<span class="text-sm font-normal">%</span>
        </span>
      </div>
    </div>

    <!-- Variant 2: DIGITAL — LED-style readout + segmented bar. -->
    <div v-else-if="variant === 'digital'" class="flex h-24 w-28 flex-col items-center justify-center gap-2">
      <div class="font-mono text-3xl font-bold tabular-nums" :style="{ color }">
        {{ Math.round(value).toString().padStart(2, '0') }}
        <span class="text-base opacity-70">%</span>
      </div>
      <div class="flex h-2 w-full items-center gap-[2px]">
        <div
          v-for="(lit, i) in digitalCells"
          :key="i"
          class="h-full flex-1 rounded-[1px] transition-colors duration-300"
          :style="{ backgroundColor: lit ? color : 'var(--cn-border)' }"
        />
      </div>
    </div>

    <!-- Variant 3: ANALOG — semicircle with a needle and tick marks.
         Classic speedometer aesthetic; good for users who want the
         traditional dashboard vibe. -->
    <div v-else-if="variant === 'analog'" class="relative h-24 w-28">
      <svg viewBox="0 0 120 80" class="h-full w-full">
        <!-- Track arc -->
        <path
          d="M 10 70 A 50 50 0 0 1 110 70"
          fill="none"
          stroke="var(--cn-border)"
          stroke-width="6"
          stroke-linecap="round"
        />
        <!-- Value arc — same path, dasharray trick -->
        <path
          d="M 10 70 A 50 50 0 0 1 110 70"
          fill="none"
          :stroke="color"
          stroke-width="6"
          stroke-linecap="round"
          :stroke-dasharray="157"
          :stroke-dashoffset="157 - (157 * clamped) / 100"
          class="transition-[stroke-dashoffset] duration-700 ease-out"
        />
        <!-- Tick marks every 20% -->
        <g stroke="var(--cn-text-muted)" stroke-width="1.5">
          <line
            v-for="n in 6"
            :key="n"
            :x1="60 + 44 * Math.cos(Math.PI + ((n - 1) / 5) * Math.PI)"
            :y1="70 + 44 * Math.sin(Math.PI + ((n - 1) / 5) * Math.PI)"
            :x2="60 + 50 * Math.cos(Math.PI + ((n - 1) / 5) * Math.PI)"
            :y2="70 + 50 * Math.sin(Math.PI + ((n - 1) / 5) * Math.PI)"
          />
        </g>
        <!-- Needle -->
        <g :transform="`rotate(${needleAngle} 60 70)`" class="transition-transform duration-700 ease-out">
          <line x1="60" y1="70" x2="60" y2="22" :stroke="color" stroke-width="2.5" stroke-linecap="round" />
          <circle cx="60" cy="70" r="4" :fill="color" />
        </g>
      </svg>
      <div class="absolute bottom-0 left-0 right-0 text-center text-lg font-semibold tabular-nums text-[var(--cn-text)]">
        {{ Math.round(value) }}%
      </div>
    </div>

    <!-- Variant 4: COCKPIT — Peugeot i-Cockpit-inspired neon ring
         with 240° sweep, outer glow, and a floating numeric readout.
         Feels like a car instrument cluster; great for night mode. -->
    <div v-else-if="variant === 'cockpit'" class="relative h-24 w-24">
      <svg viewBox="0 0 120 120" class="h-full w-full">
        <defs>
          <filter :id="`cockpit-glow-${label}`" x="-50%" y="-50%" width="200%" height="200%">
            <feGaussianBlur stdDeviation="2.5" result="blur" />
            <feMerge>
              <feMergeNode in="blur" />
              <feMergeNode in="SourceGraphic" />
            </feMerge>
          </filter>
        </defs>
        <!-- Track: dim full sweep -->
        <path :d="cockpitTrackPath" fill="none" stroke="rgb(56 97 141 / 0.35)" stroke-width="5" stroke-linecap="round" />
        <!-- Value: bright arc with glow filter -->
        <path
          :d="cockpitArcPath"
          fill="none"
          :stroke="color"
          stroke-width="4"
          stroke-linecap="round"
          :filter="`url(#cockpit-glow-${label})`"
          class="transition-all duration-700 ease-out"
        />
        <!-- Tick marks along the sweep -->
        <g stroke="rgb(56 189 248 / 0.45)" stroke-width="1">
          <line
            v-for="n in 11"
            :key="n"
            :x1="60 + 40 * Math.cos(Math.PI + (((COCKPIT_START + ((n - 1) / 10) * COCKPIT_SWEEP) - 180) * Math.PI) / 180)"
            :y1="60 + 40 * Math.sin(Math.PI + (((COCKPIT_START + ((n - 1) / 10) * COCKPIT_SWEEP) - 180) * Math.PI) / 180)"
            :x2="60 + 52 * Math.cos(Math.PI + (((COCKPIT_START + ((n - 1) / 10) * COCKPIT_SWEEP) - 180) * Math.PI) / 180)"
            :y2="60 + 52 * Math.sin(Math.PI + (((COCKPIT_START + ((n - 1) / 10) * COCKPIT_SWEEP) - 180) * Math.PI) / 180)"
          />
        </g>
      </svg>
      <div class="absolute inset-0 flex flex-col items-center justify-center">
        <span
          class="text-2xl font-bold tabular-nums drop-shadow-[0_0_6px_rgba(56,189,248,0.6)]"
          :style="{ color }"
        >
          {{ Math.round(value) }}
        </span>
        <span class="text-[9px] uppercase tracking-[0.15em]" :style="{ color }">%</span>
      </div>
    </div>

    <!-- Shared label + subtext below every variant. -->
    <div class="mt-2 text-xs font-medium text-[var(--cn-text-muted)]">{{ label }}</div>
    <div v-if="subtext" class="text-[11px] text-[var(--cn-text-muted)]">{{ subtext }}</div>
  </div>
</template>
