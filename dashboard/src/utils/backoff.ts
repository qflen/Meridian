// Exponential-backoff schedule for reconnect attempts.
//
// The schedule is kept as a pair of pure functions so the timing can be
// unit-tested without a live socket: `backoffBaseDelay` is the deterministic
// geometric series (1s, 2s, 4s, … capped), and `applyJitter` spreads each
// delay over a random window so a fleet of clients does not reconnect in
// lockstep after a shared outage.

export interface BackoffOptions {
  /** Delay for the first retry, in ms. */
  baseMs?: number;
  /** Upper bound on any single delay, in ms. */
  capMs?: number;
  /** Geometric growth factor per attempt. */
  factor?: number;
  /** Fraction (0..1) of each delay that is randomised away. */
  jitter?: number;
}

const DEFAULTS: Required<BackoffOptions> = {
  baseMs: 1000,
  capMs: 30000,
  factor: 2,
  jitter: 0.5,
};

/** Deterministic delay for `attempt` (0-based), before jitter, capped at `capMs`. */
export function backoffBaseDelay(attempt: number, opts: BackoffOptions = {}): number {
  const { baseMs, capMs, factor } = { ...DEFAULTS, ...opts };
  const a = Number.isFinite(attempt) ? Math.max(0, Math.floor(attempt)) : 0;
  const raw = baseMs * Math.pow(factor, a);
  return Math.min(raw, capMs);
}

/**
 * Spread `baseDelay` over `[baseDelay*(1-jitter), baseDelay)`. With the default
 * jitter of 0.5 a delay keeps at least half its nominal value and adds up to
 * half at random, so it never exceeds the cap nor collapses to zero.
 */
export function applyJitter(
  baseDelay: number,
  jitter = DEFAULTS.jitter,
  rand: () => number = Math.random,
): number {
  const j = Math.min(Math.max(jitter, 0), 1);
  if (j === 0) return baseDelay;
  const floor = baseDelay * (1 - j);
  return floor + rand() * (baseDelay * j);
}

/** Full jittered delay for `attempt`. */
export function nextReconnectDelay(
  attempt: number,
  opts: BackoffOptions = {},
  rand: () => number = Math.random,
): number {
  return applyJitter(backoffBaseDelay(attempt, opts), opts.jitter ?? DEFAULTS.jitter, rand);
}
