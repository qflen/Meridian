// One shared set of formatters so identical quantities render identically
// everywhere — the dashboard previously carried three different `formatBytes`
// and two `formatVal`/`formatValue` copies with mismatched precision.

const BYTE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];

/**
 * Human byte size with one fixed precision and unit set (decimal / SI, where
 * 1 KB = 1000 B): one decimal for scaled units, none for raw bytes. `--` for
 * non-finite input.
 */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes)) return '--';
  const sign = bytes < 0 ? '-' : '';
  let n = Math.abs(bytes);
  if (n < 1000) return `${sign}${Math.round(n)} B`;
  let unit = 0;
  while (n >= 1000 && unit < BYTE_UNITS.length - 1) {
    n /= 1000;
    unit++;
  }
  return `${sign}${n.toFixed(1)} ${BYTE_UNITS[unit]}`;
}

/**
 * Compact number for axes, legends, and live values. Scaled units (K/M/G/T)
 * carry one decimal so columns align; whole numbers below 1000 print exactly;
 * very small magnitudes use exponential so they never collapse to a flat `0`.
 */
export function formatNumber(value: number): string {
  if (!Number.isFinite(value)) return '--';
  const a = Math.abs(value);
  if (a >= 1e12) return `${(value / 1e12).toFixed(1)}T`;
  if (a >= 1e9) return `${(value / 1e9).toFixed(1)}G`;
  if (a >= 1e6) return `${(value / 1e6).toFixed(1)}M`;
  if (a >= 1e3) return `${(value / 1e3).toFixed(1)}K`;
  if (a > 0 && a < 0.01) return value.toExponential(1);
  return value % 1 === 0 ? String(value) : value.toFixed(2);
}

/**
 * Compact duration from a second count: `45s`, `5m`, `1h 30m`, `2d 3h`. `--`
 * for negative or non-finite input.
 */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '--';
  const s = Math.floor(seconds);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) {
    const rm = m % 60;
    return rm ? `${h}h ${rm}m` : `${h}h`;
  }
  const d = Math.floor(h / 24);
  const rh = h % 24;
  return rh ? `${d}d ${rh}h` : `${d}d`;
}

/**
 * Local wall-clock time. Seconds are included by default (live readouts); pass
 * `{ seconds: false }` for coarser HH:MM axis ticks.
 */
export function formatTime(ts: number, opts: { seconds?: boolean } = {}): string {
  const withSeconds = opts.seconds !== false;
  return new Date(ts).toLocaleTimeString([], {
    hour: '2-digit',
    minute: '2-digit',
    ...(withSeconds ? { second: '2-digit' } : {}),
  });
}
