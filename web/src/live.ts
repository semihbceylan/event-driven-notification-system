// WebSocket connection to GET /v1/ws with automatic reconnect.

import { ref } from "vue";
import type { StatusEvent } from "./api";

export type LiveState = "connecting" | "live" | "reconnecting";

/**
 * Connects to the status stream and calls onEvent for every message.
 * onReconnect runs after a dropped connection comes back, because events
 * sent while disconnected are lost: the caller should reload from the API.
 */
export function useLiveStatus(onEvent: (ev: StatusEvent) => void, onReconnect: () => void) {
  const state = ref<LiveState>("connecting");
  let socket: WebSocket | null = null;
  let retryDelay = 500;
  let stopped = false;
  let everConnected = false;

  function connect() {
    const proto = location.protocol === "https:" ? "wss" : "ws";
    socket = new WebSocket(`${proto}://${location.host}/v1/ws`);

    socket.onopen = () => {
      state.value = "live";
      retryDelay = 500;
      if (everConnected) onReconnect();
      everConnected = true;
    };
    socket.onmessage = (msg) => {
      try {
        onEvent(JSON.parse(msg.data) as StatusEvent);
      } catch {
        // ignore malformed messages
      }
    };
    socket.onclose = () => {
      if (stopped) return;
      state.value = "reconnecting";
      setTimeout(connect, retryDelay);
      retryDelay = Math.min(retryDelay * 2, 10_000); // back off, like the worker does
    };
  }

  connect();
  return {
    state,
    stop() {
      stopped = true;
      socket?.close();
    },
  };
}
