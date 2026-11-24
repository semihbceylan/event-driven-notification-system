// The live feed: notifications loaded from the API, kept up to date by
// WebSocket events. The merge logic is kept free of Vue so it can be unit
// tested on its own.

import type { Notification, StatusEvent } from "./api";

export interface Row {
  id: string;
  batch_id?: string;
  channel: Notification["channel"];
  priority: Notification["priority"];
  recipient: string;
  status: Notification["status"];
  attempts: number;
  last_error?: string;
  /** Time of the latest change we know about (ISO 8601). */
  at: string;
}

export const MAX_ROWS = 300;

/**
 * The status to show. A pending notification that already has a failed
 * attempt is waiting to be retried; showing it as plain "pending" with a
 * red error next to it reads as a failure, which it is not.
 */
export function displayStatus(r: Pick<Row, "status" | "attempts" | "last_error">): string {
  if (r.status === "pending" && r.attempts > 0 && r.last_error) return "retrying";
  return r.status;
}

export function rowFromNotification(n: Notification): Row {
  return {
    id: n.id,
    batch_id: n.batch_id,
    channel: n.channel,
    priority: n.priority,
    recipient: n.recipient,
    status: n.status,
    attempts: n.attempts,
    last_error: n.last_error,
    at: n.updated_at,
  };
}

/**
 * Applies one status event and returns the new list. Events come from
 * several processes (API and workers) and can arrive out of order, so an
 * event older than what we already have for that notification is ignored.
 * New notifications go to the top, and the list is capped at MAX_ROWS.
 */
export function applyEvent(rows: Row[], ev: StatusEvent): Row[] {
  const i = rows.findIndex((r) => r.id === ev.id);
  const next: Row = {
    id: ev.id,
    batch_id: ev.batch_id,
    channel: ev.channel,
    priority: ev.priority,
    recipient: ev.recipient,
    status: ev.status,
    attempts: ev.attempts,
    last_error: ev.last_error,
    at: ev.at,
  };

  if (i === -1) {
    return [next, ...rows].slice(0, MAX_ROWS);
  }
  if (Date.parse(ev.at) < Date.parse(rows[i].at)) {
    return rows; // stale
  }
  const copy = rows.slice();
  copy[i] = { ...next, last_error: ev.last_error ?? (ev.status === "sent" ? undefined : rows[i].last_error) };
  return copy;
}
