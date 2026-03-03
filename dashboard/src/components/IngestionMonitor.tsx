import { useDashboard } from '../state/DashboardContext';
import { TimeSeriesChart } from './TimeSeriesChart';
import { Panel } from './Panel';
import { Placeholder } from './Placeholder';
import { useRef, useEffect, useState } from 'react';
import { Sample } from '../types';
import { formatBytes, formatNumber } from '../utils/format';
import { PANEL_BODY } from '../utils/layout';

export function IngestionMonitor() {
  const { state } = useDashboard();
  const stats = state.stats;
  const [rateHistory, setRateHistory] = useState<Sample[]>([]);
  const [dropRate, setDropRate] = useState(0);
  const lastUpdateRef = useRef(0);
  const lastDropRef = useRef<{ dropped: number; ts: number } | null>(null);

  useEffect(() => {
    // Record every stats tick, including idle periods. The ingestion rate now
    // genuinely decays to 0 at idle, and `!stats.ingestionRate` treated that
    // legitimate 0 as "no data", leaving gaps in the throughput chart.
    if (!stats) return;
    const now = Date.now();
    if (now - lastUpdateRef.current < 1000) return;
    lastUpdateRef.current = now;

    setRateHistory((prev) => {
      const next = [...prev, { timestamp: now, value: stats.ingestionRate }];
      return next.length > 120 ? next.slice(-120) : next;
    });

    // Derive a per-second shed rate from the cumulative drop counter (the frame
    // carries the cumulative total, like samples_ingested_total; see ADR-023).
    const last = lastDropRef.current;
    if (last && now > last.ts) {
      const delta = stats.droppedSamples - last.dropped;
      setDropRate(delta > 0 ? (delta * 1000) / (now - last.ts) : 0);
    }
    lastDropRef.current = { dropped: stats.droppedSamples, ts: now };
  }, [stats]);

  const depth = stats?.ingestQueueDepth ?? 0;
  const capacity = stats?.ingestQueueCapacity ?? 0;
  const highWater = stats?.ingestQueueHighWatermark ?? 0;
  const dropped = stats?.droppedSamples ?? 0;
  const pct = capacity > 0 ? Math.min(100, (depth / capacity) * 100) : 0;
  // The bar reads neutral until the high-water mark (early backpressure), warns
  // through the throttle band, and goes critical when the queue is full (shedding).
  const barColor =
    capacity > 0 && depth >= capacity
      ? 'bg-crit'
      : highWater > 0 && depth >= highWater
        ? 'bg-warn'
        : 'bg-accent';
  const dropColor = dropped > 0 ? 'text-warn' : 'text-muted';

  return (
    <Panel tier="secondary" title="Ingestion Monitor" bodyHeight={PANEL_BODY.monitor}>
      <div className="grid grid-cols-2 sm:grid-cols-4 gap-3 mb-3">
        <div>
          <div className="stat-value">{stats ? stats.ingestionRate.toLocaleString() : '--'}</div>
          <div className="stat-label">samples/sec</div>
        </div>
        <div>
          <div className="stat-value">{stats ? stats.activeSeries.toLocaleString() : '--'}</div>
          <div className="stat-label">active series</div>
        </div>
        <div>
          <div className="stat-value">{stats ? formatBytes(stats.memoryBytes) : '--'}</div>
          <div className="stat-label">memory</div>
        </div>
        <div>
          <div className="stat-value">{stats ? stats.walSegments : '--'}</div>
          <div className="stat-label">WAL segments</div>
        </div>
      </div>

      {/* Write-path backpressure (ADR-023): bounded queue depth vs capacity, plus
          cumulative drops and the derived shed rate. A spike fills the bar; once it
          reaches the cap, load is shed and the drop counters move. */}
      <div className="mb-3">
        <div className="flex items-baseline justify-between gap-2 mb-1">
          <span className="stat-label">ingest queue</span>
          <span className="font-mono text-2xs tabular-nums text-muted">
            <span className="text-text">{stats ? formatNumber(depth) : '--'}</span>
            {' / '}
            {stats ? formatNumber(capacity) : '--'} samples
          </span>
        </div>
        <div
          className="h-1.5 w-full rounded-full bg-border/50 overflow-hidden"
          role="meter"
          aria-label="ingest queue depth"
          aria-valuemin={0}
          aria-valuemax={capacity}
          aria-valuenow={depth}
        >
          <div className={`h-full ${barColor}`} style={{ width: `${pct}%` }} />
        </div>
        <div className="flex items-baseline justify-between gap-2 mt-1">
          <span className="font-mono text-2xs tabular-nums text-muted">
            drops <span className={dropColor}>{formatNumber(dropped)}</span>
          </span>
          {dropRate > 0 && (
            <span className="font-mono text-2xs tabular-nums text-crit">{formatNumber(dropRate)}/s shed</span>
          )}
        </div>
      </div>

      <div className="flex-1 min-h-0">
        {rateHistory.length > 0 ? (
          <TimeSeriesChart
            series={[{ label: 'Ingestion Rate', samples: rateHistory }]}
            showLegend={false}
            yLabel="samples/s"
            title="Ingestion Throughput"
          />
        ) : (
          <Placeholder title="No throughput recorded yet." hint="The ingestion rate plots here once samples arrive." />
        )}
      </div>
    </Panel>
  );
}
