import { describe, it, expect } from 'vitest';
import { parseWSMessage } from './wsMessage';

describe('parseWSMessage', () => {
  it('passes through a complete stats frame', () => {
    const frame = {
      type: 'stats',
      ingestionRate: 100,
      activeSeries: 5,
      memoryBytes: 1,
      compressedBytes: 2,
      rawBytes: 3,
      walSegments: 1,
      blockCount: 0,
      uptimeSeconds: 42,
    };
    expect(parseWSMessage(frame)).toEqual(frame);
  });

  it('defaults missing or non-finite stats fields to 0 (no NaN reaches the UI)', () => {
    const msg = parseWSMessage({ type: 'stats', uptimeSeconds: 'oops' });
    expect(msg).not.toBeNull();
    if (msg && msg.type === 'stats') {
      expect(msg.uptimeSeconds).toBe(0);
      expect(msg.ingestionRate).toBe(0);
      expect(Number.isNaN(msg.uptimeSeconds)).toBe(false);
    }
  });

  it('accepts a well-formed metric frame', () => {
    const frame = {
      type: 'metric',
      series: 'cpu',
      labels: { host: 'a' },
      timestamp: 10,
      value: 3.5,
    };
    expect(parseWSMessage(frame)).toEqual(frame);
  });

  it('drops metric frames with no series id or a non-finite point', () => {
    expect(parseWSMessage({ type: 'metric', timestamp: 1, value: 2 })).toBeNull();
    expect(parseWSMessage({ type: 'metric', series: 'x', timestamp: NaN, value: 2 })).toBeNull();
    expect(parseWSMessage({ type: 'metric', series: 'x', timestamp: 1, value: 'bad' })).toBeNull();
  });

  it('rejects unknown shapes and non-objects', () => {
    expect(parseWSMessage(null)).toBeNull();
    expect(parseWSMessage('hello')).toBeNull();
    expect(parseWSMessage({ type: 'bogus' })).toBeNull();
    expect(parseWSMessage({})).toBeNull();
  });
});
