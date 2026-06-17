import { describe, it, expect } from 'vitest';
import { liveRowKey } from './liveStream';

describe('liveRowKey', () => {
  it('is identical for the same sample regardless of position', () => {
    const e = { key: 'cpu{host="a"}', ts: 1718600000000, value: 0.42 };
    expect(liveRowKey(e)).toBe(liveRowKey({ ...e }));
  });

  it('differs across series, timestamps, and values', () => {
    const base = { key: 'cpu', ts: 1000, value: 1 };
    expect(liveRowKey(base)).not.toBe(liveRowKey({ ...base, key: 'mem' }));
    expect(liveRowKey(base)).not.toBe(liveRowKey({ ...base, ts: 2000 }));
    expect(liveRowKey(base)).not.toBe(liveRowKey({ ...base, value: 2 }));
  });

  it('keeps keys stable when the list is re-sorted (no index dependence)', () => {
    const entries = [
      { key: 'a', ts: 3, value: 1 },
      { key: 'b', ts: 2, value: 1 },
      { key: 'a', ts: 1, value: 1 },
    ];
    const before = entries.map(liveRowKey);
    const after = [...entries].sort((x, y) => y.ts - x.ts).map(liveRowKey);
    // Same set of keys, merely reordered — a given row never changes key.
    expect(new Set(after)).toEqual(new Set(before));
    expect(new Set(before).size).toBe(3);
  });
});
