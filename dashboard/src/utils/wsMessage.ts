import { WSMessage, TimeSeries } from '../types';

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
        walSegments: num(obj.walSegments),
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

    default:
      return null;
  }
}
