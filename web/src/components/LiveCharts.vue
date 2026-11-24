<script setup lang="ts">
// Sends per second (from WebSocket events) and the queue backlog (from the
// shared metrics poller), over the last three minutes.
import { computed, onMounted, onUnmounted, shallowRef } from "vue";
import { CHANNELS, type Channel } from "../api";
import { breakerBands, RateCounter, WINDOW_SECONDS, type Sample, type Series } from "../series";
import TimeChart, { type ChartBand, type Point } from "./TimeChart.vue";

const props = defineProps<{
  rates: RateCounter;
  samples: Sample[];
  rateLimit: number | null;
}>();

// The counter is plain (not reactive), so events cost nothing in Vue. A
// one-second tick takes a snapshot; it stops while the tab is hidden.
const now = shallowRef(Date.now());
const snap = shallowRef<Series>(props.rates.snapshot(now.value));
let timer: number | undefined;
function tick() {
  now.value = Date.now();
  snap.value = props.rates.snapshot(now.value);
}
function start() {
  if (timer !== undefined) return;
  tick();
  timer = window.setInterval(tick, 1000);
}
function stop() {
  window.clearInterval(timer);
  timer = undefined;
}
const onVisibility = () => (document.hidden ? stop() : start());
onMounted(() => {
  document.addEventListener("visibilitychange", onVisibility);
  if (!document.hidden) start();
});
onUnmounted(() => {
  document.removeEventListener("visibilitychange", onVisibility);
  stop();
});

const ratePoints = computed(() => {
  const out = {} as Record<Channel, Point[]>;
  for (const c of CHANNELS) out[c] = snap.value[c].map((value, k) => ({ ago: WINDOW_SECONDS - k, value }));
  return out;
});

const agoOf = (at: number) => Math.max(0, (now.value - at) / 1000);

const queuePoints = computed(() => {
  const out = {} as Record<Channel, Point[]>;
  const visible = props.samples.filter((s) => agoOf(s.at) <= WINDOW_SECONDS);
  for (const c of CHANNELS) out[c] = visible.map((s) => ({ ago: agoOf(s.at), value: s.due[c] }));
  return out;
});

const bands = computed<ChartBand[]>(() =>
  breakerBands(props.samples)
    .map((b) => ({
      fromAgo: Math.min(WINDOW_SECONDS, agoOf(b.from)),
      toAgo: agoOf(b.to),
      label: `${b.channels.join(", ")} breaker open`,
    }))
    .filter((b) => b.toAgo < WINDOW_SECONDS),
);

const reference = computed(() =>
  props.rateLimit ? { value: props.rateLimit, label: `limit ${props.rateLimit}/s` } : null,
);
</script>

<template>
  <section class="charts">
    <TimeChart
      title="Sends per second"
      :points="ratePoints"
      unit="/s"
      :min-max="10"
      :bands="bands"
      :reference="reference"
      empty="Nothing sent in the last 3 minutes. Try a burst."
    >
      <p class="muted small chart-note">Each channel is rate limited separately; a burst runs flat at the limit.</p>
    </TimeChart>
    <TimeChart
      title="Queue backlog (due now)"
      :points="queuePoints"
      unit=" due"
      :min-max="10"
      :bands="bands"
      empty="The queue is empty."
    >
      <p class="muted small chart-note">While a breaker is open, messages wait in the queue instead of failing.</p>
    </TimeChart>
  </section>
</template>
