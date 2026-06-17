import { describe, it, expect } from 'vitest';
import { compressionRatio } from './compression';

describe('compressionRatio', () => {
  it('computes raw/compressed when both are positive', () => {
    expect(compressionRatio(1000, 100)).toBe(10);
    expect(compressionRatio(2800, 100)).toBeCloseTo(28);
  });

  it('returns a finite 0 at cold start when compressedBytes is 0', () => {
    const r = compressionRatio(1000, 0);
    expect(r).toBe(0);
    expect(Number.isFinite(r)).toBe(true); // never Infinity ("Infinityx")
  });

  it('returns 0 when rawBytes is 0', () => {
    expect(compressionRatio(0, 100)).toBe(0);
  });

  it('returns 0 for negative or non-finite inputs', () => {
    expect(compressionRatio(-1, 100)).toBe(0);
    expect(compressionRatio(1000, -5)).toBe(0);
    expect(compressionRatio(NaN, 100)).toBe(0);
    expect(compressionRatio(1000, Infinity)).toBe(0);
  });
});
