import { describe, expect, it } from "vitest";
import { applyEvent, MAX_ROWS, type Row } from "./feed";
import type { StatusEvent } from "./api";

const ev = (over: Partial<StatusEvent>): StatusEvent => ({
  id: "a",
  channel: "sms",
  priority: "normal",
  recipient: "+905551234567",
  status: "pending",
  attempts: 0,
  at: "2026-11-23T12:00:00.000Z",
  ...over,
});

describe("applyEvent", () => {
  it("adds unknown notifications to the top", () => {
    const rows = applyEvent([], ev({ id: "a" }));
    const next = applyEvent(rows, ev({ id: "b" }));
    expect(next.map((r) => r.id)).toEqual(["b", "a"]);
  });

  it("updates a known notification in place", () => {
    let rows = applyEvent([], ev({ id: "a" }));
    rows = applyEvent(rows, ev({ id: "b" }));
    rows = applyEvent(rows, ev({ id: "a", status: "sent", attempts: 1, at: "2026-11-23T12:00:01.000Z" }));
    expect(rows.map((r) => r.id)).toEqual(["b", "a"]);
    expect(rows[1]).toMatchObject({ status: "sent", attempts: 1 });
  });

  it("ignores an event older than the current state", () => {
    // The worker's "processing" can arrive before the API's "created".
    let rows = applyEvent([], ev({ status: "processing", attempts: 1, at: "2026-11-23T12:00:00.500Z" }));
    rows = applyEvent(rows, ev({ status: "pending", attempts: 0, at: "2026-11-23T12:00:00.100Z" }));
    expect(rows[0].status).toBe("processing");
  });

  it("keeps the last error while retrying and clears it once sent", () => {
    let rows = applyEvent([], ev({ status: "pending", attempts: 1, last_error: "503", at: "2026-11-23T12:00:01Z" }));
    rows = applyEvent(rows, ev({ status: "processing", attempts: 2, at: "2026-11-23T12:00:02Z" }));
    expect(rows[0].last_error).toBe("503");
    rows = applyEvent(rows, ev({ status: "sent", attempts: 2, at: "2026-11-23T12:00:03Z" }));
    expect(rows[0].last_error).toBeUndefined();
  });

  it("caps the list", () => {
    let rows: Row[] = [];
    for (let i = 0; i < MAX_ROWS + 10; i++) rows = applyEvent(rows, ev({ id: String(i) }));
    expect(rows).toHaveLength(MAX_ROWS);
    expect(rows[0].id).toBe(String(MAX_ROWS + 9));
  });
});
