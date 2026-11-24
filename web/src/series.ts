// Time series for the live charts. Kept free of Vue so it can be unit
// tested, and cheap on purpose: the dashboard may receive hundreds of
// events per second during a burst, so recording one is O(1) with no
// allocation, and the charts only read a snapshot once per second.

import { CHANNELS, type Channel } from "./api";

export type Series = Record<Channel, number[]>;

/** How much history the charts show, in seconds. */
export const WINDOW_SECONDS = 180;

/**
 * Counts events per channel per wall-clock second in a fixed ring of
 * buckets. Each bucket remembers which second it holds, so a bucket left
 * over from a previous lap of the ring reads as zero.
 */
export class RateCounter {
  private counts: Record<Channel, Uint32Array>;
  private stamps: Record<Channel, Float64Array>;

  constructor(private size = WINDOW_SECONDS) {
    this.counts = {} as Record<Channel, Uint32Array>;
    this.stamps = {} as Record<Channel, Float64Array>;
    for (const c of CHANNELS) {
      this.counts[c] = new Uint32Array(size);
      this.stamps[c] = new Float64Array(size).fill(-1);
    }
  }

  add(channel: Channel, atMs: number) {
    const counts = this.counts[channel];
    if (!counts) return; // unknown channel from a newer server
    const sec = Math.floor(atMs / 1000);
    const i = sec % this.size;
    if (this.stamps[channel][i] !== sec) {
      this.stamps[channel][i] = sec;
      counts[i] = 0;
    }
    counts[i]++;
  }

  /**
   * Per-second counts for the last `size` complete seconds before nowMs,
   * oldest first. The current second is left out because it is still
   * filling up and would always look like a drop.
   */
  snapshot(nowMs: number): Series {
    const last = Math.floor(nowMs / 1000) - 1;
    const first = last - this.size + 1;
    const out = {} as Series;
    for (const c of CHANNELS) {
      const values = new Array<number>(this.size);
      for (let k = 0; k < this.size; k++) {
        const sec = first + k;
        const i = ((sec % this.size) + this.size) % this.size;
        values[k] = this.stamps[c][i] === sec ? this.counts[c][i] : 0;
      }
      out[c] = values;
    }
    return out;
  }
}

/** One reading of the metrics endpoint, kept for the backlog chart. */
export interface Sample {
  at: number; // ms
  due: Record<Channel, number>;
  /** Channels whose circuit breaker was not closed at this reading. */
  open: Channel[];
}

/** Adds a sample and drops those older than the window. */
export function pushSample(samples: Sample[], s: Sample, windowMs = WINDOW_SECONDS * 1000): Sample[] {
  const cutoff = s.at - windowMs;
  const kept = samples.filter((x) => x.at >= cutoff);
  kept.push(s);
  return kept;
}

export interface Band {
  from: number; // ms
  to: number; // ms
  channels: Channel[];
}

/**
 * Turns per-sample breaker states into time ranges where at least one
 * breaker was open, for shading the charts. A range ends at the first
 * sample where no breaker is open, or at the last sample.
 */
export function breakerBands(samples: Sample[]): Band[] {
  const bands: Band[] = [];
  let cur: Band | null = null;
  for (const s of samples) {
    if (s.open.length > 0) {
      if (!cur) {
        cur = { from: s.at, to: s.at, channels: [] };
        bands.push(cur);
      }
      cur.to = s.at;
      for (const c of s.open) if (!cur.channels.includes(c)) cur.channels.push(c);
    } else if (cur) {
      cur.to = s.at;
      cur = null;
    }
  }
  return bands;
}

/** A y-axis maximum and ticks that cover the data with round numbers. */
export function niceScale(max: number, minMax: number): { max: number; ticks: number[] } {
  const target = Math.max(max, minMax);
  const rough = target / 4;
  const pow = 10 ** Math.floor(Math.log10(rough));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * pow).find((s) => s >= rough) ?? 10 * pow;
  const top = Math.ceil(target / step) * step;
  const ticks: number[] = [];
  for (let v = 0; v <= top + step / 2; v += step) ticks.push(Math.round(v * 100) / 100);
  return { max: top, ticks };
}
