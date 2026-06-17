import { describe, it, expect } from 'vitest';
import { backoffBaseDelay, applyJitter, nextReconnectDelay } from './backoff';

describe('backoffBaseDelay', () => {
  it('doubles each attempt from a 1s base', () => {
    expect(backoffBaseDelay(0)).toBe(1000);
    expect(backoffBaseDelay(1)).toBe(2000);
    expect(backoffBaseDelay(2)).toBe(4000);
    expect(backoffBaseDelay(3)).toBe(8000);
    expect(backoffBaseDelay(4)).toBe(16000);
  });

  it('caps at 30s', () => {
    expect(backoffBaseDelay(5)).toBe(30000); // 32000 -> capped
    expect(backoffBaseDelay(6)).toBe(30000);
    expect(backoffBaseDelay(50)).toBe(30000);
  });

  it('is non-decreasing across attempts', () => {
    let prev = 0;
    for (let a = 0; a <= 12; a++) {
      const d = backoffBaseDelay(a);
      expect(d).toBeGreaterThanOrEqual(prev);
      prev = d;
    }
  });

  it('clamps negative or non-finite attempts to the base', () => {
    expect(backoffBaseDelay(-3)).toBe(1000);
    expect(backoffBaseDelay(NaN)).toBe(1000);
  });

  it('honors custom options', () => {
    expect(backoffBaseDelay(0, { baseMs: 500 })).toBe(500);
    // 500 * 3^3 = 13500, capped at 5000
    expect(backoffBaseDelay(3, { baseMs: 500, factor: 3, capMs: 5000 })).toBe(5000);
  });
});

describe('applyJitter', () => {
  it('spreads a delay over [base*(1-j), base) for the default jitter', () => {
    const base = 8000;
    expect(applyJitter(base, 0.5, () => 0)).toBe(4000);
    expect(applyJitter(base, 0.5, () => 0.5)).toBe(6000);
    expect(applyJitter(base, 0.5, () => 0.999999)).toBeLessThan(base);
    expect(applyJitter(base, 0.5, () => 0.999999)).toBeGreaterThan(4000);
  });

  it('returns the base unchanged when jitter is 0', () => {
    expect(applyJitter(8000, 0, () => 0.123)).toBe(8000);
  });

  it('never exceeds the base delay', () => {
    for (const r of [0, 0.25, 0.5, 0.75, 0.9999]) {
      expect(applyJitter(1000, 0.5, () => r)).toBeLessThanOrEqual(1000);
    }
  });
});

describe('nextReconnectDelay', () => {
  it('stays within [0, capMs] for every attempt and random value', () => {
    for (let a = 0; a < 20; a++) {
      for (const r of [0, 0.5, 0.9999]) {
        const d = nextReconnectDelay(a, {}, () => r);
        expect(d).toBeGreaterThanOrEqual(0);
        expect(d).toBeLessThanOrEqual(30000);
      }
    }
  });

  it('lands the first retry within ~1s', () => {
    expect(nextReconnectDelay(0, {}, () => 0)).toBe(500);
    expect(nextReconnectDelay(0, {}, () => 0.999)).toBeLessThan(1000);
  });
});
