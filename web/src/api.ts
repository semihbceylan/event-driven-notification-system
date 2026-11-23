// Typed client for the notification API. Paths are relative: nginx (or the
// Vite dev server) proxies /v1 to the API, /worker to the worker and /mock
// to the mock provider.

export type Channel = "sms" | "email" | "push";
export type Priority = "high" | "normal" | "low";
export type Status = "pending" | "processing" | "sent" | "failed" | "cancelled";

export const CHANNELS: Channel[] = ["sms", "email", "push"];
export const STATUSES: Status[] = ["pending", "processing", "sent", "failed", "cancelled"];

export interface Notification {
  id: string;
  batch_id?: string;
  channel: Channel;
  priority: Priority;
  recipient: string;
  content: string;
  status: Status;
  attempts: number;
  last_error?: string;
  updated_at: string;
}

/** One message from GET /v1/ws. */
export interface StatusEvent {
  id: string;
  batch_id?: string;
  channel: Channel;
  priority: Priority;
  recipient: string;
  status: Status;
  attempts: number;
  last_error?: string;
  at: string;
}

export interface Percentiles {
  p50: number | null;
  p95: number | null;
  p99: number | null;
}

export interface ChannelStats {
  queue: { due: number; waiting: number; processing: number };
  sent: number;
  failed: number;
  success_rate: number | null;
  attempts: number;
  attempt_success_rate: number | null;
  provider_latency_ms: Percentiles;
  delivery_latency_ms: Percentiles;
}

export interface CreateInput {
  recipient: string;
  channel: Channel;
  content: string;
  priority: Priority;
  scheduled_at?: string;
}

export interface FieldError {
  field: string;
  message: string;
}

/** An error response from the API, with field details for validation errors. */
export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public details: FieldError[] = [],
  ) {
    super(message);
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { "Content-Type": "application/json", ...init?.headers },
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) {
    const e = body?.error ?? {};
    throw new ApiError(res.status, e.code ?? "error", e.message ?? res.statusText, e.details ?? []);
  }
  return body as T;
}

export const api = {
  list: (limit = 100) =>
    request<{ data: Notification[]; next_cursor: string | null }>(`/v1/notifications?limit=${limit}`),
  create: (input: CreateInput) =>
    request<Notification>("/v1/notifications", { method: "POST", body: JSON.stringify(input) }),
  createBatch: (items: CreateInput[]) =>
    request<{ batch_id: string; total: number; created: number }>("/v1/notifications/batch", {
      method: "POST",
      body: JSON.stringify({ notifications: items }),
    }),
  cancel: (id: string) => request<Notification>(`/v1/notifications/${id}/cancel`, { method: "POST" }),
  retry: (id: string) => request<Notification>(`/v1/notifications/${id}/retry`, { method: "POST" }),
  metrics: () =>
    request<{ window: string; channels: Record<Channel, ChannelStats> }>("/v1/metrics?window=5m"),
  breakers: () => request<Record<Channel, string>>("/worker/breakers"),
  mockConfig: () => request<{ config: { failure_rate: number; failure_status: number } }>("/mock/admin/config"),
  setMockConfig: (cfg: { failure_rate: number; failure_status?: number }) =>
    request("/mock/admin/config", { method: "PUT", body: JSON.stringify(cfg) }),
};

/** A recipient that passes validation for the channel, for demo bursts. */
export function sampleRecipient(channel: Channel, i: number): string {
  switch (channel) {
    case "sms":
      return `+9055${String(10000000 + i).slice(-8)}`;
    case "email":
      return `user${i}@example.com`;
    case "push":
      return `device-token-${i}`;
  }
}
