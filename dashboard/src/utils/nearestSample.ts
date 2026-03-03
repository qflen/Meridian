import { Sample } from '../types';

/**
 * Index of the sample whose timestamp is closest to `t`, by binary search.
 *
 * The crosshair readout calls this once per series per pointer move to find the
 * value under the cursor, so it must stay O(log n). Samples are assumed sorted
 * ascending by timestamp — true for query results and the live ring buffer,
 * which only ever append in time order. Ties (a target exactly between two
 * samples) resolve to the earlier sample. Returns -1 for an empty series.
 */
export function nearestSampleIndex(samples: Sample[], t: number): number {
  const n = samples.length;
  if (n === 0) return -1;
  if (t <= samples[0].timestamp) return 0;
  if (t >= samples[n - 1].timestamp) return n - 1;

  // Lower-bound search: first index whose timestamp is >= t.
  let lo = 0;
  let hi = n - 1;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (samples[mid].timestamp < t) lo = mid + 1;
    else hi = mid;
  }

  const after = lo;
  const before = lo - 1;
  // `<=` keeps the earlier sample on an exact midpoint tie.
  return t - samples[before].timestamp <= samples[after].timestamp - t ? before : after;
}

/** The sample nearest `t`, or null for an empty series. */
export function nearestSample(samples: Sample[], t: number): Sample | null {
  const i = nearestSampleIndex(samples, t);
  return i < 0 ? null : samples[i];
}
