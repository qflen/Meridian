import { describe, it, expect } from 'vitest';
import { formatBytes, formatNumber, formatDuration, formatTime, formatMillis } from './format';

describe('formatBytes', () => {
  it('prints raw bytes with no decimal', () => {
    expect(formatBytes(0)).toBe('0 B');
    expect(formatBytes(512)).toBe('512 B');
    expect(formatBytes(999)).toBe('999 B');
  });

  it('uses decimal (SI) units with one decimal place', () => {
    expect(formatBytes(1000)).toBe('1.0 KB');
    expect(formatBytes(1536)).toBe('1.5 KB');
    expect(formatBytes(1_234_000)).toBe('1.2 MB');
    expect(formatBytes(1_200_000_000)).toBe('1.2 GB');
    expect(formatBytes(2_500_000_000_000)).toBe('2.5 TB');
  });

  it('renders the same quantity identically regardless of caller', () => {
    // The bug this replaces: memory read "1.2 GB" while raw size read "1.23 GB".
    const q = 1_230_000_000;
    expect(formatBytes(q)).toBe(formatBytes(q));
    expect(formatBytes(q)).toBe('1.2 GB');
  });

  it('handles sign and non-finite input', () => {
    expect(formatBytes(-2048)).toBe('-2.0 KB');
    expect(formatBytes(NaN)).toBe('--');
    expect(formatBytes(Infinity)).toBe('--');
  });
});

describe('formatNumber', () => {
  it('prints whole numbers below 1000 exactly', () => {
    expect(formatNumber(0)).toBe('0');
    expect(formatNumber(42)).toBe('42');
    expect(formatNumber(999)).toBe('999');
  });

  it('prints fractional sub-1000 values with two decimals', () => {
    expect(formatNumber(50.3)).toBe('50.30');
    expect(formatNumber(3.14159)).toBe('3.14');
  });

  it('scales with one decimal and a unit suffix', () => {
    expect(formatNumber(1500)).toBe('1.5K');
    expect(formatNumber(12_000_000)).toBe('12.0M');
    expect(formatNumber(1e9)).toBe('1.0G');
    expect(formatNumber(3.2e12)).toBe('3.2T');
    expect(formatNumber(-1500)).toBe('-1.5K');
  });

  it('uses exponential for very small magnitudes instead of a flat 0', () => {
    expect(formatNumber(0.005)).toBe('5.0e-3');
  });

  it('returns -- for non-finite input', () => {
    expect(formatNumber(NaN)).toBe('--');
    expect(formatNumber(-Infinity)).toBe('--');
  });
});

describe('formatDuration', () => {
  it('formats seconds, minutes, hours, and days', () => {
    expect(formatDuration(0)).toBe('0s');
    expect(formatDuration(45)).toBe('45s');
    expect(formatDuration(60)).toBe('1m');
    expect(formatDuration(90)).toBe('1m');
    expect(formatDuration(3600)).toBe('1h');
    expect(formatDuration(5400)).toBe('1h 30m');
    expect(formatDuration(86_400)).toBe('1d');
    expect(formatDuration(90_000)).toBe('1d 1h');
  });

  it('returns -- for negative or non-finite input', () => {
    expect(formatDuration(-5)).toBe('--');
    expect(formatDuration(NaN)).toBe('--');
  });
});

describe('formatTime', () => {
  // Locale/timezone vary across machines, so assert structure (colon-separated
  // fields) rather than exact digits.
  const ts = Date.UTC(2026, 0, 2, 9, 5, 7);

  it('includes seconds by default (three fields)', () => {
    expect(formatTime(ts).split(':').length).toBe(3);
  });

  it('omits seconds when asked (two fields)', () => {
    expect(formatTime(ts, { seconds: false }).split(':').length).toBe(2);
  });
});

describe('formatMillis', () => {
  it('rounds to the precision a reader can use', () => {
    expect(formatMillis(0.4)).toBe('<1 ms');
    expect(formatMillis(3.456)).toBe('3.5 ms');
    expect(formatMillis(135.334)).toBe('135 ms');
    expect(formatMillis(1234)).toBe('1.23 s');
  });

  it('prints -- for bad input', () => {
    expect(formatMillis(NaN)).toBe('--');
    expect(formatMillis(-1)).toBe('--');
  });
});
