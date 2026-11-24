<script setup lang="ts">
// A small SVG line chart, one line per channel, over the last few minutes.
// Plain SVG, no chart library: three polylines of at most 180 points,
// redrawn at most once a second.
import { computed, ref } from "vue";
import { CHANNELS, type Channel } from "../api";
import { niceScale, WINDOW_SECONDS } from "../series";

export interface Point {
  ago: number; // seconds before now
  value: number;
}
export interface ChartBand {
  fromAgo: number;
  toAgo: number;
  label: string;
}

const props = defineProps<{
  title: string;
  points: Record<Channel, Point[]>;
  unit: string;
  minMax: number;
  bands?: ChartBand[];
  reference?: { value: number; label: string } | null;
  empty?: string;
}>();

const W = 560;
const H = 220;
const M = { l: 40, r: 48, t: 16, b: 24 };
const NAME: Record<Channel, string> = { sms: "SMS", email: "Email", push: "Push" };

const x = (ago: number) => M.l + (1 - ago / WINDOW_SECONDS) * (W - M.l - M.r);

const scale = computed(() => {
  let max = props.reference?.value ?? 0;
  for (const c of CHANNELS) for (const p of props.points[c]) if (p.value > max) max = p.value;
  return niceScale(max * 1.15, props.minMax); // headroom for the band label above the top line
});
const y = (v: number) => H - M.b - (v / scale.value.max) * (H - M.t - M.b);

const hasData = computed(() => CHANNELS.some((c) => props.points[c].some((p) => p.value > 0)));

const lines = computed(() =>
  CHANNELS.map((c) => ({
    c,
    d: props.points[c].map((p) => `${x(p.ago).toFixed(1)},${y(p.value).toFixed(1)}`).join(" "),
  })),
);

// Endpoint dots with direct labels. Labels are nudged upward from the
// lowest one, so they never overlap each other or drop below the axis.
const ends = computed(() => {
  const out = CHANNELS.flatMap((c) => {
    const pts = props.points[c];
    const last = pts[pts.length - 1];
    return last ? [{ c, cx: x(last.ago), cy: y(last.value), ly: y(last.value) }] : [];
  }).sort((a, b) => b.ly - a.ly);
  for (let i = 1; i < out.length; i++) if (out[i - 1].ly - out[i].ly < 12) out[i].ly = out[i - 1].ly - 12;
  return out;
});

// The right edge is "now"; the endpoint labels sit there, so it has no tick label.
const xTicks = [180, 120, 60].map((ago) => ({ ago, label: `-${ago / 60}m` }));

// Hover: crosshair and tooltip at the nearest reading.
const svgEl = ref<SVGSVGElement | null>(null);
const hover = ref<{ i: number; left: number; flip: boolean } | null>(null);
const base = computed(() => props.points[CHANNELS[0]]);

function onMove(e: PointerEvent) {
  const r = svgEl.value?.getBoundingClientRect();
  if (!r || base.value.length === 0) return;
  const px = ((e.clientX - r.left) / r.width) * W;
  let best = 0;
  for (let i = 1; i < base.value.length; i++) {
    if (Math.abs(x(base.value[i].ago) - px) < Math.abs(x(base.value[best].ago) - px)) best = i;
  }
  const left = (x(base.value[best].ago) / W) * r.width;
  hover.value = { i: best, left, flip: left > r.width * 0.6 };
}

const fmtAgo = (ago: number) => (ago < 1 ? "now" : `${Math.round(ago)} s ago`);
</script>

<template>
  <section class="panel chart">
    <div class="chart-head">
      <h2>{{ title }}</h2>
      <div class="legend">
        <span v-for="c in CHANNELS" :key="c" :class="`c-${c}`">{{ NAME[c] }}</span>
      </div>
    </div>
    <div class="plot">
      <svg ref="svgEl" :viewBox="`0 0 ${W} ${H}`" role="img" :aria-label="title">
        <g v-for="(b, i) in bands" :key="i">
          <rect class="band" :x="x(b.fromAgo)" :y="M.t" :width="Math.max(2, x(b.toAgo) - x(b.fromAgo))" :height="H - M.t - M.b" />
          <text v-if="x(b.toAgo) - x(b.fromAgo) > 70" class="band-label" :x="(x(b.fromAgo) + x(b.toAgo)) / 2" :y="M.t + 12" text-anchor="middle">
            {{ b.label }}
          </text>
        </g>
        <g v-for="t in scale.ticks" :key="t">
          <line class="gridline" :x1="M.l" :x2="W - M.r" :y1="y(t)" :y2="y(t)" />
          <text :x="M.l - 8" :y="y(t) + 4" text-anchor="end">{{ t }}</text>
        </g>
        <text v-for="t in xTicks" :key="t.ago" :x="x(t.ago)" :y="H - 6" text-anchor="middle">{{ t.label }}</text>
        <g v-if="reference">
          <line class="ref" :x1="M.l" :x2="W - M.r" :y1="y(reference.value)" :y2="y(reference.value)" />
          <text :x="M.l + 4" :y="y(reference.value) - 5">{{ reference.label }}</text>
        </g>
        <polyline v-for="l in lines" :key="l.c" :class="`line c-${l.c}`" :points="l.d" />
        <g v-for="e in ends" :key="e.c">
          <circle :class="`dot c-${e.c}`" :cx="e.cx" :cy="e.cy" r="4" />
          <text class="end-label" :x="e.cx + 8" :y="e.ly + 4">{{ NAME[e.c] }}</text>
        </g>
        <g v-if="hover">
          <line class="cross" :x1="x(base[hover.i].ago)" :x2="x(base[hover.i].ago)" :y1="M.t" :y2="H - M.b" />
          <template v-for="c in CHANNELS" :key="c">
            <circle v-if="points[c][hover.i]" :class="`dot c-${c}`" :cx="x(points[c][hover.i].ago)" :cy="y(points[c][hover.i].value)" r="4" />
          </template>
        </g>
        <text v-if="!hasData" class="empty" :x="(M.l + W - M.r) / 2" :y="H / 2" text-anchor="middle">{{ empty }}</text>
        <rect class="hit" :x="M.l" y="0" :width="W - M.l - M.r" :height="H" @pointermove="onMove" @pointerleave="hover = null" />
      </svg>
      <div v-if="hover" class="tip" :style="{ left: `${hover.flip ? hover.left - 12 : hover.left + 12}px`, transform: hover.flip ? 'translateX(-100%)' : 'none' }">
        <b>{{ fmtAgo(base[hover.i].ago) }}</b>
        <div v-for="c in CHANNELS" :key="c">
          <i :class="`c-${c}`"></i>{{ NAME[c] }} {{ points[c][hover.i]?.value ?? 0 }}{{ unit }}
        </div>
      </div>
    </div>
    <slot />
  </section>
</template>
