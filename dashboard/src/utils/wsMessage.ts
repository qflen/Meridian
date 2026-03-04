import { WSMessage, TimeSeries, Anomaly } from '../types';

// Validation/normalisation for inbound WebSocket frames. A malformed or
// partial payload must never reach the UI as a NaN (which renders as e.g.
// "Up NaNm"): unknown shapes are dropped, and numeric stats fields are coerced
// to finite numbers with a safe default.

function isFiniteNum(v: unknown): v is number {
  return typeof v === 'number' && Number.isFinite(v);
}

function num(v: unknown, fallback = 0): number {
  return isFiniteNum(v) ? v : fallback;
}

function str(v: unknown, fallback = ''): string {
  return typeof v === 'string' ? v : fallback;
}

/**
 * Validate and normalise one anomaly record (from a WebSocket frame or the
 * /api/v1/anomalies seed). Requires a series id and finite value/timestamp; the
 * severity/state enums and numeric fields are coerced to safe defaults. Returns
 * `null` for shapes that are not anomalies.
 */
export function coerceAnomaly(raw: unknown): Anomaly | null {
  if (!raw || typeof raw !== 'object') return null;
  const o = raw as Record<string, unknown>;
  if (typeof o.series !== 'string') return null;
  if (!isFiniteNum(o.timestamp) || !isFiniteNum(o.value)) return null;
  return {
    seq: num(o.seq),
    series: o.series,
    metric: str(o.metric),
    labels: o.labels && typeof o.labels === 'object' ? (o.labels as Record<string, string>) : {},
    value: o.value,
    baseline: num(o.baseline),
    score: num(o.score),
    severity: o.severity === 'crit' ? 'crit' : 'warn',
    state: o.state === 'resolved' ? 'resolved' : 'firing',
    timestamp: o.timestamp,
  };
}

/**
 * Parse and validate a decoded WebSocket message. Returns a well-formed
 * `WSMessage`, or `null` if the payload is not a message shape we understand.
 */
export function parseWSMessage(raw: unknown): WSMessage | null {
  if (!raw || typeof raw !== 'object') return null;
  const obj = raw as Record<string, unknown>;

  switch (obj.type) {
    case 'stats':
      return {
        type: 'stats',
        ingestionRate: num(obj.ingestionRate),
        activeSeries: num(obj.activeSeries),
        memoryBytes: num(obj.memoryBytes),
        compressedBytes: num(obj.compressedBytes),
        rawBytes: num(obj.rawBytes),
        walBytes: num(obj.walBytes),
        blockCount: num(obj.blockCount),
        uptimeSeconds: num(obj.uptimeSeconds),
        ingestQueueDepth: num(obj.ingestQueueDepth),
        ingestQueueCapacity: num(obj.ingestQueueCapacity),
        ingestQueueHighWatermark: num(obj.ingestQueueHighWatermark),
        droppedSamples: num(obj.droppedSamples),
      };

    case 'metric':
      // A metric with no series id or a non-finite point would only pollute the
      // live stream (e.g. a sample stamped at the epoch), so drop it entirely.
      if (typeof obj.series !== 'string') return null;
      if (!isFiniteNum(obj.timestamp) || !isFiniteNum(obj.value)) return null;
      return {
        type: 'metric',
        series: obj.series,
        labels:
          obj.labels && typeof obj.labels === 'object'
            ? (obj.labels as Record<string, string>)
            : {},
        timestamp: obj.timestamp,
        value: obj.value,
      };

    case 'live':
      if (!Array.isArray(obj.series)) return null;
      return { type: 'live', series: obj.series as TimeSeries[] };

    case 'anomaly': {
      const a = coerceAnomaly(obj);
      return a ? { type: 'anomaly', ...a } : null;
    }

    default:
      return null;
  }
}
