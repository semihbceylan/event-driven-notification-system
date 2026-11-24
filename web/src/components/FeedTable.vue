<script setup lang="ts">
import { computed, ref } from "vue";
import { api, ApiError, CHANNELS, STATUSES, type Channel, type Status } from "../api";
import { displayStatus, type Row } from "../feed";
import type { LiveState } from "../live";

const props = defineProps<{ rows: Row[]; live: LiveState }>();

const statusFilter = ref<Status | "">("");
const channelFilter = ref<Channel | "">("");
const actionError = ref("");

const visible = computed(() =>
  props.rows.filter(
    (r) => (!statusFilter.value || r.status === statusFilter.value) && (!channelFilter.value || r.channel === channelFilter.value),
  ),
);

async function act(kind: "cancel" | "retry", id: string) {
  actionError.value = "";
  try {
    await (kind === "cancel" ? api.cancel(id) : api.retry(id));
    // No local update needed: the change comes back over the WebSocket.
  } catch (e) {
    actionError.value = e instanceof ApiError ? e.message : (e as Error).message;
  }
}

const time = (iso: string) => new Date(iso).toLocaleTimeString();

// Errors wrap to two lines; clicking one shows it in full.
const expanded = ref(new Set<string>());
function toggle(id: string) {
  const next = new Set(expanded.value);
  if (!next.delete(id)) next.add(id);
  expanded.value = next;
}
</script>

<template>
  <section class="panel feed">
    <header class="feed-header">
      <h2>Live feed</h2>
      <span class="live" :class="live">{{ live === "live" ? "● live" : live === "connecting" ? "connecting…" : "reconnecting…" }}</span>
      <div class="filters">
        <select v-model="channelFilter" aria-label="Filter by channel">
          <option value="">All channels</option>
          <option v-for="c in CHANNELS" :key="c" :value="c">{{ c }}</option>
        </select>
        <select v-model="statusFilter" aria-label="Filter by status">
          <option value="">All statuses</option>
          <option v-for="s in STATUSES" :key="s" :value="s">{{ s }}</option>
        </select>
      </div>
    </header>

    <p v-if="actionError" class="banner error">{{ actionError }}</p>

    <div class="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Updated</th>
            <th>Channel</th>
            <th>Priority</th>
            <th>Recipient</th>
            <th>Status</th>
            <th class="num">Attempts</th>
            <th>Last error</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in visible" :key="r.id">
            <td class="muted">{{ time(r.at) }}</td>
            <td>{{ r.channel }}</td>
            <td><span class="prio" :class="r.priority">{{ r.priority }}</span></td>
            <td class="mono">{{ r.recipient }}</td>
            <td><span class="status-pill" :class="displayStatus(r)">{{ displayStatus(r) }}</span></td>
            <td class="num">{{ r.attempts }}</td>
            <td class="error-cell" :class="{ soft: r.status !== 'failed' }">
              <button
                v-if="r.last_error"
                type="button"
                class="error-text"
                :class="{ open: expanded.has(r.id) }"
                :title="expanded.has(r.id) ? 'Click to collapse' : r.last_error"
                :aria-expanded="expanded.has(r.id)"
                @click="toggle(r.id)"
              >
                {{ r.last_error }}
              </button>
            </td>
            <td class="actions">
              <button v-if="r.status === 'pending'" class="link" @click="act('cancel', r.id)">Cancel</button>
              <button v-if="r.status === 'failed'" class="link" @click="act('retry', r.id)">Retry</button>
            </td>
          </tr>
          <tr v-if="visible.length === 0">
            <td colspan="8" class="muted empty">No notifications yet. Send one, or try a burst.</td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>
