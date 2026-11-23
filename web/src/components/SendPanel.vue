<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";
import { api, ApiError, CHANNELS, sampleRecipient, type Channel, type CreateInput, type Priority } from "../api";

const form = reactive<CreateInput & { schedule: string }>({
  recipient: "+905551234567",
  channel: "sms",
  content: "Flash sale: 50% off for the next hour!",
  priority: "normal",
  schedule: "",
});
const placeholders: Record<Channel, string> = {
  sms: "+905551234567",
  email: "user@example.com",
  push: "device-token-abc123",
};

const busy = ref(false);
const message = ref<{ kind: "ok" | "error"; text: string; details?: string[] } | null>(null);

function onChannelChange() {
  form.recipient = placeholders[form.channel];
}

async function send() {
  busy.value = true;
  message.value = null;
  try {
    const input: CreateInput = {
      recipient: form.recipient,
      channel: form.channel,
      content: form.content,
      priority: form.priority,
    };
    if (form.schedule) input.scheduled_at = new Date(form.schedule).toISOString();
    const n = await api.create(input);
    message.value = { kind: "ok", text: `Queued ${n.channel} notification ${n.id.slice(0, 8)}…` };
  } catch (e) {
    showError(e);
  } finally {
    busy.value = false;
  }
}

// Sends a mixed batch to show priorities, rate limiting and the live feed.
async function burst(count: number) {
  busy.value = true;
  message.value = null;
  const priorities: Priority[] = ["high", "normal", "low"];
  const items: CreateInput[] = Array.from({ length: count }, (_, i) => {
    const channel = CHANNELS[i % CHANNELS.length];
    return {
      channel,
      recipient: sampleRecipient(channel, i),
      content: `Burst message ${i + 1} of ${count}`,
      priority: priorities[i % priorities.length],
    };
  });
  try {
    const res = await api.createBatch(items);
    message.value = { kind: "ok", text: `Batch ${res.batch_id.slice(0, 8)}…: ${res.created} queued across 3 channels` };
  } catch (e) {
    showError(e);
  } finally {
    busy.value = false;
  }
}

function showError(e: unknown) {
  if (e instanceof ApiError) {
    message.value = { kind: "error", text: e.message, details: e.details.map((d) => `${d.field}: ${d.message}`) };
  } else {
    message.value = { kind: "error", text: (e as Error).message };
  }
}

// Mock provider switch: makes the circuit breaker easy to demonstrate.
const providerDown = ref<boolean | null>(null);
async function loadProvider() {
  try {
    providerDown.value = (await api.mockConfig()).config.failure_rate >= 1;
  } catch {
    providerDown.value = null;
  }
}
async function toggleProvider() {
  const down = !providerDown.value;
  await api.setMockConfig(down ? { failure_rate: 1, failure_status: 503 } : { failure_rate: 0 });
  providerDown.value = down;
}
onMounted(loadProvider);
</script>

<template>
  <section class="panel">
    <h2>Send</h2>
    <form @submit.prevent="send">
      <div class="row">
        <label>
          Channel
          <select v-model="form.channel" @change="onChannelChange">
            <option v-for="c in CHANNELS" :key="c" :value="c">{{ c }}</option>
          </select>
        </label>
        <label>
          Priority
          <select v-model="form.priority">
            <option value="high">high</option>
            <option value="normal">normal</option>
            <option value="low">low</option>
          </select>
        </label>
      </div>
      <label>
        Recipient
        <input v-model="form.recipient" :placeholder="placeholders[form.channel]" required />
      </label>
      <label>
        Content
        <textarea v-model="form.content" rows="3" required></textarea>
      </label>
      <label>
        Schedule for (optional)
        <input v-model="form.schedule" type="datetime-local" />
      </label>
      <button type="submit" :disabled="busy">Send notification</button>
    </form>

    <div class="burst">
      <span class="muted">Load test:</span>
      <button type="button" class="secondary" :disabled="busy" @click="burst(100)">Burst 100</button>
      <button type="button" class="secondary" :disabled="busy" @click="burst(1000)">Burst 1000</button>
    </div>

    <p v-if="message" class="banner" :class="message.kind">
      {{ message.text }}
      <ul v-if="message.details?.length">
        <li v-for="d in message.details" :key="d">{{ d }}</li>
      </ul>
    </p>

    <div v-if="providerDown !== null" class="provider">
      <div>
        <strong>Mock provider</strong>
        <span class="status-pill" :class="providerDown ? 'failed' : 'sent'">{{ providerDown ? "down (503)" : "up" }}</span>
        <p class="muted small">
          Take it down to watch the circuit breaker pause sending instead of failing messages. Only affects
          sends when <code>PROVIDER_URL</code> is not set to webhook.site.
        </p>
      </div>
      <button type="button" class="secondary" @click="toggleProvider">
        {{ providerDown ? "Bring provider back" : "Simulate outage" }}
      </button>
    </div>
  </section>
</template>
