import { describe, expect, it } from "vitest";
import { breakerBands, niceScale, pushSample, RateCounter, type Sample } from "./series";

const T0 = 1_800_000_000_000; // a whole second, in ms

describe("RateCounter", () => {
  it("counts events per channel per second, oldest first", () => {
    const r = new RateCounter(5);
    r.add("sms", T0);
    r.add("sms", T0 + 999);
    r.add("email", T0 + 1000);
    const s = r.snapshot(T0 + 2500); // complete seconds: T0-3s .. T0+1s
    expect(s.sms).toEqual([0, 0, 0, 2, 0]);
    expect(s.email).toEqual([0, 0, 0, 0, 1]);
    expect(s.push).toEqual([0, 0, 0, 0, 0]);
  });

  it("leaves out the second that is still filling up", () => {
    const r = new RateCounter(3);
    r.add("sms", T0 + 100);
    expect(r.snapshot(T0 + 500).sms).toEqual([0, 0, 0]);
  });

  it("does not show counts from a previous lap of the ring", () => {
    const r = new RateCounter(3);
    r.add("sms", T0);
    // Three seconds later the same bucket holds a new second.
    r.add("sms", T0 + 3000);
    expect(r.snapshot(T0 + 4000).sms).toEqual([0, 0, 1]);
    // Long after, everything has aged out.
    expect(r.snapshot(T0 + 60_000).sms).toEqual([0, 0, 0]);
  });

  it("ignores channels it does not know", () => {
    const r = new RateCounter(3);
    expect(() => r.add("fax" as never, T0)).not.toThrow();
  });
});

const sample = (at: number, open: Sample["open"] = []): Sample => ({ at, due: { sms: 0, email: 0, push: 0 }, open });

describe("pushSample", () => {
  it("drops samples older than the window", () => {
    let s: Sample[] = [];
    s = pushSample(s, sample(0), 10_000);
    s = pushSample(s, sample(5_000), 10_000);
    s = pushSample(s, sample(12_000), 10_000);
    expect(s.map((x) => x.at)).toEqual([5_000, 12_000]);
  });
});

describe("breakerBands", () => {
  it("joins consecutive open readings into one band per outage", () => {
    const bands = breakerBands([
      sample(0),
      sample(2, ["email"]),
      sample(4, ["email", "sms"]),
      sample(6),
      sample(8, ["push"]),
    ]);
    expect(bands).toEqual([
      { from: 2, to: 6, channels: ["email", "sms"] },
      { from: 8, to: 8, channels: ["push"] },
    ]);
  });
});

describe("niceScale", () => {
  it("rounds the top up and uses round ticks", () => {
    expect(niceScale(100, 10)).toEqual({ max: 100, ticks: [0, 25, 50, 75, 100] });
    expect(niceScale(1000, 10)).toEqual({ max: 1000, ticks: [0, 250, 500, 750, 1000] });
    expect(niceScale(37, 10).max).toBeGreaterThanOrEqual(37);
  });

  it("keeps a minimum so an idle chart is not all noise", () => {
    expect(niceScale(0, 10).max).toBe(10);
  });
});
