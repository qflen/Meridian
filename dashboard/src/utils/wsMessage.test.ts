import { describe, it, expect } from 'vitest';
import { parseWSMessage, coerceAnomaly } from './wsMessage';

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
      ingestQueueDepth: 120,
      ingestQueueCapacity: 50000,
      ingestQueueHighWatermark: 40000,
      droppedSamples: 7,
    };
    expect(parseWSMessage(frame)).toEqual(frame);
  });

  it('defaults missing or non-finite stats fields to 0 (no NaN reaches the UI)', () => {
    const msg = parseWSMessage({ type: 'stats', uptimeSeconds: 'oops' });
    expect(msg).not.toBeNull();
    if (msg && msg.type === 'stats') {
      expect(msg.uptimeSeconds).toBe(0);
      expect(msg.ingestionRate).toBe(0);
      // Backpressure fields default cleanly when an older server omits them.
      expect(msg.ingestQueueDepth).toBe(0);
      expect(msg.ingestQueueCapacity).toBe(0);
      expect(msg.droppedSamples).toBe(0);
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

  it('accepts a well-formed anomaly frame', () => {
    const frame = {
      type: 'anomaly',
      seq: 12,
      series: 'cpu_usage_percent{host="web-01"}',
      metric: 'cpu_usage_percent',
      labels: { host: 'web-01' },
      value: 95.2,
      baseline: 41.3,
      score: 8.4,
      severity: 'crit',
      state: 'firing',
      timestamp: 1700,
    };
    expect(parseWSMessage(frame)).toEqual(frame);
  });

  it('drops anomaly frames with no series or a non-finite point', () => {
    expect(parseWSMessage({ type: 'anomaly', value: 1, timestamp: 1 })).toBeNull();
    expect(parseWSMessage({ type: 'anomaly', series: 'x', value: NaN, timestamp: 1 })).toBeNull();
    expect(parseWSMessage({ type: 'anomaly', series: 'x', value: 1, timestamp: 'bad' })).toBeNull();
  });
});

describe('coerceAnomaly', () => {
  it('coerces unknown severity/state and missing numbers to safe defaults', () => {
    const a = coerceAnomaly({ series: 's', value: 5, timestamp: 10, severity: 'bogus', state: 'bogus' });
    expect(a).not.toBeNull();
    if (a) {
      expect(a.severity).toBe('warn'); // unknown severity → warn
      expect(a.state).toBe('firing'); // unknown state → firing
      expect(a.seq).toBe(0);
      expect(a.baseline).toBe(0);
      expect(a.score).toBe(0);
      expect(a.labels).toEqual({});
      expect(a.metric).toBe('');
    }
  });

  it('keeps a valid resolved/warn record intact', () => {
    const a = coerceAnomaly({
      seq: 3, series: 's', metric: 'm', labels: { a: 'b' },
      value: 1, baseline: 2, score: 3, severity: 'warn', state: 'resolved', timestamp: 9,
    });
    expect(a).toEqual({
      seq: 3, series: 's', metric: 'm', labels: { a: 'b' },
      value: 1, baseline: 2, score: 3, severity: 'warn', state: 'resolved', timestamp: 9,
    });
  });

  it('rejects non-anomaly shapes', () => {
    expect(coerceAnomaly(null)).toBeNull();
    expect(coerceAnomaly({ value: 1, timestamp: 1 })).toBeNull(); // no series
    expect(coerceAnomaly({ series: 's', timestamp: 1 })).toBeNull(); // no finite value
  });
});
