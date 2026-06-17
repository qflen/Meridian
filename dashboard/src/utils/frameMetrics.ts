// Frame-drop accounting for the FPS readout.
//
// Two failure modes the previous lifetime counter had:
//  - a single early hiccup stayed on screen forever (never reset);
//  - a backgrounded tab throttles requestAnimationFrame to ~1Hz, so the gap on
//    return looked like hundreds of "dropped" frames.
// Both are addressed by counting per-window and ignoring implausibly large
// deltas (which signal a hidden tab, not a slow frame).

export const TARGET_FRAME_MS = 1000 / 60;
export const MAX_PLAUSIBLE_FRAME_MS = 250;

/**
 * Dropped frames implied by a single inter-frame delta. A delta within one
 * frame of the target counts as none; a delta larger than `maxPlausibleMs` is
 * treated as a hidden/throttled tab and ignored.
 */
export function droppedFramesForDelta(
  delta: number,
  targetMs: number = TARGET_FRAME_MS,
  maxPlausibleMs: number = MAX_PLAUSIBLE_FRAME_MS,
): number {
  if (!Number.isFinite(delta) || delta <= 0) return 0;
  if (delta > maxPlausibleMs) return 0; // tab hidden / throttled — not real drops
  if (delta <= targetMs * 2) return 0; // within one frame of target
  return Math.floor(delta / targetMs) - 1;
}

/** Total dropped frames across a window of inter-frame deltas. */
export function droppedFramesInWindow(
  deltas: number[],
  targetMs: number = TARGET_FRAME_MS,
  maxPlausibleMs: number = MAX_PLAUSIBLE_FRAME_MS,
): number {
  let total = 0;
  for (const d of deltas) total += droppedFramesForDelta(d, targetMs, maxPlausibleMs);
  return total;
}
