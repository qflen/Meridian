/**
 * Vertical sizing scale (px, on an 8px module).
 *
 * One scale for every panel body and canvas, so panels within a tier line up
 * and the chrome reads as deliberate — replacing the eyeballed 140 / 160 / 180 /
 * 256 / 294 px heights each panel previously picked on its own. Panels apply a
 * value as their fixed body height (`<Panel bodyHeight>`); the chart or list
 * inside fills that box with `flex-1`, so sizing is intrinsic below the body.
 */
export const PANEL_BODY = {
  /** Primary query-result instrument — the dominant surface. */
  signature: 360,
  /** Secondary monitor row (ingestion, cluster topology). */
  monitor: 240,
  /** Secondary live-stream feed — a wide, shorter ticker. */
  ticker: 208,
  /** Tertiary readout row (compression, latency, retention). */
  compact: 176,
  /** Tertiary metric explorer (list + preview). */
  explorer: 248,
} as const;
