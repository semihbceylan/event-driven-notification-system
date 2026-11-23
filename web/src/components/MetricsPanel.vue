<script setup lang="ts">
import { onMounted, onUnmounted, ref } from "vue";
import { api, CHANNELS, type Channel, type ChannelStats } from "../api";

// Polls /v1/metrics and the worker's breaker states every 2 seconds.
const stats = ref<Record<Channel, ChannelStats> | null>(null);
const breakers = ref<Record<string, string>>({});
const error = ref("");
let timer: number | undefined;

async function refresh() {
  try {
    stats.value = (await api.metrics()).channels;
    error.value = "";
  } catch (e) {
    error.value = (e as Error).message;
  }
  try {
    breakers.value = await api.breakers();
  } catch {
    breakers.value = {}; // the worker may be restarting; not worth an error banner
  }
}

onMounted(() => {
  refresh();
  timer = window.setInterval(refresh, 2000);
});
onUnmounted(() => window.clearInterval(timer));

const pct = (v: number | null) => (v === null ? "–" : `${(v * 100).toFixed(1)}%`);
const ms = (v: number | null) => (v === null ? "–" : v >= 1000 ? `${(v / 1000).toFixed(1)} s` : `${Math.round(v)} ms`);
</script>

<template>
  <section class="metrics">
    <p v-if="error" class="banner error">Metrics unavailable: {{ error }}</p>
    <article v-for="ch in CHANNELS" :key="ch" class="card">
      <header>
        <h3>{{ ch.toUpperCase() }}</h3>
        <span
          v-if="breakers[ch]"
          class="breaker"
          :class="breakers[ch]"
          :title="breakers[ch] === 'closed' ? 'Provider healthy' : 'Circuit breaker: sending paused while the provider recovers'"
        >
          {{ breakers[ch] === "closed" ? "healthy" : `breaker ${breakers[ch]}` }}
        </span>
      </header>
      <template v-if="stats">
        <dl class="queue">
          <div><dt>Due</dt><dd>{{ stats[ch].queue.due }}</dd></div>
          <div><dt>Waiting</dt><dd>{{ stats[ch].queue.waiting }}</dd></div>
          <div><dt>Sending</dt><dd>{{ stats[ch].queue.processing }}</dd></div>
        </dl>
        <dl class="stats">
          <div><dt>Sent (5m)</dt><dd>{{ stats[ch].sent }}</dd></div>
          <div><dt>Failed (5m)</dt><dd :class="{ bad: stats[ch].failed > 0 }">{{ stats[ch].failed }}</dd></div>
          <div><dt>Delivered</dt><dd>{{ pct(stats[ch].success_rate) }}</dd></div>
          <div><dt>Attempts OK</dt><dd>{{ pct(stats[ch].attempt_success_rate) }}</dd></div>
          <div><dt>Provider p95</dt><dd>{{ ms(stats[ch].provider_latency_ms.p95) }}</dd></div>
          <div><dt>Delivery p95</dt><dd>{{ ms(stats[ch].delivery_latency_ms.p95) }}</dd></div>
        </dl>
      </template>
      <p v-else class="muted">Loading…</p>
    </article>
  </section>
</template>
