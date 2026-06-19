/**
 * Axis ticks on "nice" round steps.
 *
 * A chart that labels its y-axis 62.58 / 49.47 / 36.36 reads as a data dump;
 * an instrument labels 0 / 20 / 40 / 60. The step is chosen from {1, 2, 2.5, 5}
 * × 10^k so that about `count` intervals cover the data, and the range is
 * widened outward to the nearest multiples, so the extreme ticks sit on the plot
 * frame instead of floating inside it.
 */
export interface NiceTicks {
  ticks: number[];
  min: number;
  max: number;
  step: number;
}

export function niceStep(range: number, count: number): number {
  const rough = range / Math.max(1, count);
  const mag = Math.pow(10, Math.floor(Math.log10(rough)));
  const r = rough / mag;
  const nice = r <= 1 ? 1 : r <= 2 ? 2 : r <= 2.5 ? 2.5 : r <= 5 ? 5 : 10;
  return nice * mag;
}

/**
 * Decimal places needed to print multiples of `step` exactly: none for 1, 2, 5
 * and their powers of ten, one more for the 2.5 rung (`0.25`, `2.5`, `250`).
 */
export function stepDecimals(step: number): number {
  const exp = Math.floor(Math.log10(step));
  const mantissa = step / Math.pow(10, exp);
  const fractional = Math.abs(mantissa - Math.round(mantissa)) > 1e-6;
  return Math.max(0, -exp + (fractional ? 1 : 0));
}

export function niceTicks(
  min: number,
  max: number,
  count: number,
  opts: { integer?: boolean } = {},
): NiceTicks {
  if (!Number.isFinite(min) || !Number.isFinite(max)) {
    return { ticks: [], min, max, step: 1 };
  }
  if (max < min) [min, max] = [max, min];
  if (max === min) {
    // A flat series still needs a band to sit in.
    const pad = Math.abs(min) * 0.1 || 1;
    min -= pad;
    max += pad;
  }
  let step = niceStep(max - min, count);
  if (opts.integer && step < 1) step = 1;
  const decimals = stepDecimals(step);
  const lo = Math.floor(min / step + 1e-9) * step;
  const hi = Math.ceil(max / step - 1e-9) * step;
  const ticks: number[] = [];
  for (let i = 0; lo + i * step <= hi + step * 1e-6; i++) {
    ticks.push(Number((lo + i * step).toFixed(decimals)));
  }
  return { ticks, min: Number(lo.toFixed(decimals)), max: Number(hi.toFixed(decimals)), step };
}

/**
 * Label a tick at the precision its step implies (`2.5`, not `2.50`), falling
 * back to compact K/M/G units once the magnitude no longer fits an axis gutter.
 */
export function formatTick(v: number, step: number): string {
  const a = Math.abs(v);
  if (a >= 1e4) {
    const units: [number, string][] = [[1e12, 'T'], [1e9, 'G'], [1e6, 'M'], [1e3, 'K']];
    for (const [div, suffix] of units) {
      if (a >= div) {
        const scaled = v / div;
        return `${Number.isInteger(scaled) ? scaled : scaled.toFixed(1)}${suffix}`;
      }
    }
  }
  return v.toFixed(stepDecimals(step));
}
