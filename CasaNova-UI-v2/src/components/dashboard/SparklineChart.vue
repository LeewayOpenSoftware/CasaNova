<script setup lang="ts">
// SparklineChart — lightweight SVG sparkline with optional area fill.
//
// Pure SVG: a <polyline> for the stroke and a <polygon> for the
// translucent fill beneath it.  Uses a viewBox so the chart scales
// responsively to any parent width.  No external chart library.

import { computed } from 'vue'

const props = withDefaults(
  defineProps<{
    data: number[]
    color?: string
    fillColor?: string
    height?: number
    width?: number
    showLastValue?: boolean
    unit?: string
    max?: number
    // capacity — total number of samples the caller's history buffer
    // can hold. When set, the sparkline uses a fixed step width based
    // on this value and right-aligns the visible samples so the chart
    // behaves like a rolling window: empty on the left while the
    // buffer fills, new samples pushing in from the right, old
    // samples sliding off once the buffer saturates. Leave at 0 for
    // the legacy "stretch to fit" behavior that compresses the curve
    // every time a new sample arrives.
    capacity?: number
  }>(),
  {
    color: 'rgb(59 130 246)',       // blue-500
    fillColor: '',
    height: 40,
    width: 200,
    showLastValue: false,
    unit: '',
    max: 0,
    capacity: 0,
  },
)

// Resolve fill color — default to the line color at 10% opacity.
const resolvedFill = computed(() =>
  props.fillColor || props.color.replace('rgb(', 'rgba(').replace(')', ', 0.1)'),
)

// The viewBox uses a fixed coordinate space; the SVG stretches to fit
// its container via CSS width:100%.
const VB_W = 200
const VB_H = 40

// Convert the data array into SVG polyline points inside the viewBox.
//
// When `capacity` is set, the step width is derived from that instead
// of `d.length - 1`. This keeps the x-axis scale stable as the buffer
// fills — new samples push in from the right rather than compressing
// the existing curve — which is what the network/CPU widgets actually
// want. See the capacity prop doc above.
const linePoints = computed(() => {
  const d = props.data
  if (d.length < 2) return ''

  const peak = props.max > 0 ? props.max : d.reduce((a, b) => (a > b ? a : b), 1)
  const denom = props.capacity > 1 ? props.capacity - 1 : d.length - 1
  const step = VB_W / denom
  // When capacity is set and the buffer isn't full yet, offset every
  // point to the right so the newest sample anchors at x=VB_W and the
  // empty portion of the chart stays on the left.
  const offset = props.capacity > 1
    ? VB_W - (d.length - 1) * step
    : 0

  return d
    .map((v, i) => {
      const x = offset + i * step
      // Leave a 4-unit top/bottom padding so the stroke doesn't clip.
      const y = VB_H - 4 - ((v / peak) * (VB_H - 8))
      return `${x.toFixed(1)},${y.toFixed(1)}`
    })
    .join(' ')
})

// Polygon points = line points + two extra corners that close the area
// down to the bottom of the viewBox.
//
// When `capacity` is set we close the fill against the offset we used
// for the first/last line point instead of the viewBox edges — this
// stops the fill from spuriously reaching to x=0 while the buffer is
// still filling, which would otherwise show an empty area under an
// invisible (and misleading) "curve from origin" segment.
const areaPoints = computed(() => {
  if (!linePoints.value) return ''
  const d = props.data
  if (props.capacity > 1 && d.length < props.capacity) {
    const denom = props.capacity - 1
    const step = VB_W / denom
    const left = VB_W - (d.length - 1) * step
    return `${left.toFixed(1)},${VB_H} ${linePoints.value} ${VB_W},${VB_H}`
  }
  return `0,${VB_H} ${linePoints.value} ${VB_W},${VB_H}`
})

// Current (last) formatted value for the optional text overlay.
const lastValue = computed(() => {
  const d = props.data
  if (d.length === 0) return '—'
  const v = d[d.length - 1]
  return Number.isInteger(v) ? `${v}${props.unit}` : `${v.toFixed(1)}${props.unit}`
})
</script>

<template>
  <div class="relative">
    <svg
      :viewBox="`0 0 ${VB_W} ${VB_H}`"
      :style="{ height: `${height}px` }"
      class="w-full"
      preserveAspectRatio="none"
    >
      <!-- Translucent area fill -->
      <polygon
        v-if="areaPoints"
        :points="areaPoints"
        :fill="resolvedFill"
      />
      <!-- Line stroke -->
      <polyline
        v-if="linePoints"
        :points="linePoints"
        fill="none"
        :stroke="color"
        stroke-width="1.5"
        stroke-linejoin="round"
        stroke-linecap="round"
      />
    </svg>
    <!-- Optional current-value overlay in the top-right corner. -->
    <span
      v-if="showLastValue"
      class="absolute right-1 top-0 text-xs font-semibold tabular-nums text-[var(--cn-text)]"
    >
      {{ lastValue }}
    </span>
  </div>
</template>
