import { describe, it, expect } from 'vitest';
import { niceTicks, niceStep, formatTick } from './ticks';

describe('niceStep', () => {
  it('picks from the 1 / 2 / 2.5 / 5 ladder', () => {
    expect(niceStep(100, 5)).toBe(20);
    expect(niceStep(60, 5)).toBe(20); // rough 12 -> 20
    expect(niceStep(10, 4)).toBe(2.5);
    expect(niceStep(0.09, 4)).toBeCloseTo(0.025);
    expect(niceStep(1000, 2)).toBe(500);
  });
});

describe('niceTicks', () => {
  it('widens the range outward onto round multiples', () => {
    const t = niceTicks(-2.98, 62.58, 5);
    expect(t.step).toBe(20);
    expect(t.min).toBe(-20);
    expect(t.max).toBe(80);
    expect(t.ticks).toEqual([-20, 0, 20, 40, 60, 80]);
  });

  it('keeps a zero baseline at zero', () => {
    const t = niceTicks(0, 172, 4);
    expect(t.min).toBe(0);
    expect(t.ticks[0]).toBe(0);
    expect(t.ticks[t.ticks.length - 1]).toBeGreaterThanOrEqual(172);
  });

  it('prints fractional steps without float noise', () => {
    const t = niceTicks(0, 1, 4);
    expect(t.ticks).toEqual([0, 0.25, 0.5, 0.75, 1]);
    const u = niceTicks(0.1, 0.4, 3);
    expect(u.ticks.every((v) => String(v).length <= 4)).toBe(true);
  });

  it('gives a flat series a band to sit in', () => {
    const t = niceTicks(172, 172, 4);
    expect(t.min).toBeLessThan(172);
    expect(t.max).toBeGreaterThan(172);
    expect(t.ticks.length).toBeGreaterThanOrEqual(3);
  });

  it('never repeats an integer tick when asked for integers', () => {
    const t = niceTicks(0, 3, 4, { integer: true });
    expect(t.ticks).toEqual([0, 1, 2, 3]);
    const u = niceTicks(0, 7, 4, { integer: true });
    expect(new Set(u.ticks).size).toBe(u.ticks.length);
  });

  it('returns nothing for non-finite input', () => {
    expect(niceTicks(NaN, 1, 4).ticks).toEqual([]);
  });
});

describe('formatTick', () => {
  it('uses the precision the step implies', () => {
    expect(formatTick(2.5, 2.5)).toBe('2.5');
    expect(formatTick(20, 20)).toBe('20');
    expect(formatTick(0.25, 0.25)).toBe('0.25');
    expect(formatTick(4, 1)).toBe('4');
    expect(formatTick(5, 5)).toBe('5');
    expect(formatTick(0.02, 0.02)).toBe('0.02');
    expect(formatTick(250, 250)).toBe('250');
  });

  it('compacts large magnitudes', () => {
    expect(formatTick(20000, 5000)).toBe('20K');
    expect(formatTick(1500000, 500000)).toBe('1.5M');
  });
});
