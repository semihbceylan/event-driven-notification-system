<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from "vue";
import {
  api,
  ApiError,
  CHANNELS,
  sampleRecipient,
  type Channel,
  type CreateInput,
  type Priority,
  type ProviderState,
} from "../api";

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

// Sends a batch on the selected channel with mixed priorities, to show
// priority ordering, rate limiting and the live feed.
async function burst(count: number) {
  busy.value = true;
  message.value = null;
  const channel = form.channel;
  const priorities: Priority[] = ["high", "normal", "low"];
  const items: CreateInput[] = Array.from({ length: count }, (_, i) => ({
    channel,
    recipient: sampleRecipient(channel, i),
    content: `Burst message ${i + 1} of ${count}`,
    priority: priorities[i % priorities.length],
  }));
  try {
    const res = await api.createBatch(items);
    message.value = { kind: "ok", text: `Batch ${res.batch_id.slice(0, 8)}…: ${res.created} ${channel} notifications queued` };
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

// --- Provider selection ---
const provider = ref<ProviderState | null>(null);
const providerError = ref("");
const mockDown = ref(false);
const activeInfo = computed(() => provider.value?.providers.find((p) => p.name === provider.value?.active));

async function loadProvider() {
  try {
    provider.value = await api.provider();
    providerError.value = "";
    if (activeInfo.value?.mock) {
      mockDown.value = (await api.mockConfig()).config.failure_rate >= 1;
    }
  } catch (e) {
    providerError.value = (e as Error).message;
  }
}

async function selectProvider(name: string) {
  try {
    provider.value = await api.setProvider(name);
    providerError.value = "";
    message.value = { kind: "ok", text: `Switched to ${name}. Workers pick it up within a second and reset their circuit breakers.` };
  } catch (e) {
    showError(e);
  }
}

async function toggleMock() {
  const down = !mockDown.value;
  await api.setMockConfig(down ? { failure_rate: 1, failure_status: 503 } : { failure_rate: 0 });
  mockDown.value = down;
}

let timer: number | undefined;
onMounted(() => {
  loadProvider();
  timer = window.setInterval(loadProvider, 5000); // another tab or the API may switch it
});
onUnmounted(() => window.clearInterval(timer));
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
      <button type="button" class="secondary" :disabled="busy" @click="burst(100)">Burst 100 {{ form.channel }}</button>
      <button type="button" class="secondary" :disabled="busy" @click="burst(1000)">Burst 1000 {{ form.channel }}</button>
    </div>
    <p v-if="activeInfo && !activeInfo.mock" class="muted small">
      Careful: a free webhook.site URL accepts only 50 requests in total, retries included.
    </p>

    <p v-if="message" class="banner" :class="message.kind">
      {{ message.text }}
      <ul v-if="message.details?.length">
        <li v-for="d in message.details" :key="d">{{ d }}</li>
      </ul>
    </p>

    <div class="provider">
      <strong>Delivery provider</strong>
      <p v-if="providerError" class="banner error">Provider unavailable: {{ providerError }}</p>
      <template v-else-if="provider">
        <div class="provider-row">
          <select
            :value="provider.active"
            aria-label="Delivery provider"
            @change="selectProvider(($event.target as HTMLSelectElement).value)"
          >
            <option v-for="p in provider.providers" :key="p.name" :value="p.name">{{ p.name }} ({{ p.host }})</option>
          </select>
        </div>
        <p v-if="provider.providers.length === 1" class="muted small">
          Set <code>WEBHOOK_SITE_URL</code> in <code>.env</code> to add webhook.site here.
        </p>

        <div v-if="activeInfo?.mock" class="provider-row">
          <span class="status-pill" :class="mockDown ? 'failed' : 'sent'">{{ mockDown ? "down (503)" : "up" }}</span>
          <button type="button" class="secondary" @click="toggleMock">
            {{ mockDown ? "Bring mock back" : "Simulate outage" }}
          </button>
        </div>
        <p v-if="activeInfo?.mock" class="muted small">
          Take the mock down to watch the circuit breaker pause sending instead of failing messages.
        </p>
      </template>
    </div>
  </section>
</template>
