import { useDashboard } from '../state/DashboardContext';
import { TimeSeriesChart } from './TimeSeriesChart';
import { Panel } from './Panel';
import { useRef, useEffect, useState } from 'react';
import { Sample } from '../types';
import { formatBytes } from '../utils/format';
import { PANEL_BODY } from '../utils/layout';

export function IngestionMonitor() {
  const { state } = useDashboard();
  const stats = state.stats;
  const [rateHistory, setRateHistory] = useState<Sample[]>([]);
  const lastUpdateRef = useRef(0);

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
  }, [stats]);

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
      <div className="flex-1 min-h-0">
        <TimeSeriesChart
          series={[{ label: 'Ingestion Rate', samples: rateHistory }]}
          showLegend={false}
          yLabel="samples/s"
          title="Ingestion Throughput"
        />
      </div>
    </Panel>
  );
}
