<script setup lang="ts">
import { onMounted, onUnmounted, ref } from "vue";
import { api } from "./api";
import { applyEvent, rowFromNotification, type Row } from "./feed";
import { useLiveStatus } from "./live";
import MetricsPanel from "./components/MetricsPanel.vue";
import SendPanel from "./components/SendPanel.vue";
import FeedTable from "./components/FeedTable.vue";

const rows = ref<Row[]>([]);
const loadError = ref("");

// Events that arrive while the initial list is loading are applied on top
// of it, so nothing is lost in the gap.
let pending: Parameters<typeof applyEvent>[1][] = [];
let loading = true;

async function load() {
  loading = true;
  try {
    const page = await api.list(100);
    let next = page.data.map(rowFromNotification);
    for (const ev of pending) next = applyEvent(next, ev);
    rows.value = next;
    loadError.value = "";
  } catch (e) {
    loadError.value = (e as Error).message;
  } finally {
    pending = [];
    loading = false;
  }
}

const live = useLiveStatus(
  (ev) => {
    if (loading) pending.push(ev);
    else rows.value = applyEvent(rows.value, ev);
  },
  load, // events were missed while disconnected: reload
);

onMounted(load);
onUnmounted(live.stop);
</script>

<template>
  <div class="layout">
    <header class="top">
      <div>
        <h1>Notifications</h1>
        <p class="muted">SMS, email and push delivery, live</p>
      </div>
      <nav>
        <a href="/docs" target="_blank" rel="noopener">API docs</a>
        <a href="http://localhost:9090" target="_blank" rel="noopener">Prometheus</a>
      </nav>
    </header>

    <MetricsPanel />

    <p v-if="loadError" class="banner error">Could not load notifications: {{ loadError }}</p>

    <main class="grid">
      <SendPanel />
      <FeedTable :rows="rows" :live="live.state.value" />
    </main>
  </div>
</template>
