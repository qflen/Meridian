import { describe, it, expect } from 'vitest';
import { nearestSampleIndex, nearestSample } from './nearestSample';
import { Sample } from '../types';

const at = (...ts: number[]): Sample[] => ts.map((timestamp) => ({ timestamp, value: timestamp / 10 }));

describe('nearestSampleIndex', () => {
  it('returns -1 for an empty series', () => {
    expect(nearestSampleIndex([], 100)).toBe(-1);
    expect(nearestSample([], 100)).toBeNull();
  });

  it('returns 0 for a single-sample series regardless of target', () => {
    const s = at(500);
    expect(nearestSampleIndex(s, 0)).toBe(0);
    expect(nearestSampleIndex(s, 500)).toBe(0);
    expect(nearestSampleIndex(s, 9999)).toBe(0);
  });

  it('clamps to the ends when the target is out of range', () => {
    const s = at(100, 200, 300, 400);
    expect(nearestSampleIndex(s, 50)).toBe(0); // before first
    expect(nearestSampleIndex(s, 9999)).toBe(3); // after last
  });

  it('finds an exact match', () => {
    const s = at(100, 200, 300, 400);
    expect(nearestSampleIndex(s, 100)).toBe(0);
    expect(nearestSampleIndex(s, 300)).toBe(2);
    expect(nearestSampleIndex(s, 400)).toBe(3);
  });

  it('picks the closer neighbour when the target falls between samples', () => {
    const s = at(100, 200, 300);
    expect(nearestSampleIndex(s, 230)).toBe(1); // closer to 200
    expect(nearestSampleIndex(s, 270)).toBe(2); // closer to 300
  });

  it('resolves an exact midpoint tie to the earlier sample', () => {
    const s = at(100, 200);
    expect(nearestSampleIndex(s, 150)).toBe(0);
  });

  it('handles irregular spacing across a larger series', () => {
    const s = at(0, 10, 11, 12, 100, 1000);
    expect(nearestSampleIndex(s, 11)).toBe(2);
    expect(nearestSampleIndex(s, 60)).toBe(4); // |60-12|=48 vs |60-100|=40 -> 100
    expect(nearestSampleIndex(s, 9)).toBe(1); // 10 closer than 0
    expect(nearestSampleIndex(s, 900)).toBe(5);
  });

  it('returns the sample, not just the index, via nearestSample', () => {
    const s = at(100, 200, 300);
    expect(nearestSample(s, 280)).toEqual({ timestamp: 300, value: 30 });
  });
});
