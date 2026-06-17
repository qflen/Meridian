export interface LiveEntry {
  key: string;
  ts: number;
  value: number;
}

/**
 * Stable React key for a live-stream row. The rows are re-sorted on every
 * update, so the key must identify the sample itself — not its position in the
 * array — or React remounts every row on each render (flicker, lost hover).
 * Value is included to disambiguate the rare same-series/same-millisecond case.
 */
export function liveRowKey(e: LiveEntry): string {
  return `${e.key}-${e.ts}-${e.value}`;
}
