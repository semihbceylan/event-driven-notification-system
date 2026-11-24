// One shared poller for /v1/metrics and the worker's breaker states. The
// metric cards and the backlog chart both read from it, so adding the
// charts adds no requests. Polling pauses while the tab is hidden.

import { onMounted, onUnmounted, ref, shallowRef } from "vue";
import { api, CHANNELS, type Channel, type ChannelStats } from "./api";
import { pushSample, type Sample } from "./series";

export const POLL_MS = 2000;

export function useMetrics() {
  const stats = ref<Record<Channel, ChannelStats> | null>(null);
  const breakers = ref<Record<string, string>>({});
  const rateLimit = ref<number | null>(null);
  const error = ref("");
  const samples = shallowRef<Sample[]>([]);
  let timer: number | undefined;

  async function refresh() {
    try {
      const m = await api.metrics();
      stats.value = m.channels;
      rateLimit.value = m.rate_limit_per_second ?? null;
      error.value = "";
    } catch (e) {
      error.value = (e as Error).message;
    }
    try {
      breakers.value = await api.breakers();
    } catch {
      breakers.value = {}; // the worker may be restarting; not worth an error banner
    }
    const s = stats.value;
    if (!s) return;
    const due = {} as Record<Channel, number>;
    for (const c of CHANNELS) due[c] = s[c]?.queue.due ?? 0;
    const open = CHANNELS.filter((c) => breakers.value[c] && breakers.value[c] !== "closed");
    samples.value = pushSample(samples.value, { at: Date.now(), due, open });
  }

  function start() {
    if (timer !== undefined) return;
    refresh();
    timer = window.setInterval(refresh, POLL_MS);
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

  return { stats, breakers, rateLimit, error, samples };
}
