import { useDashboard } from '../state/DashboardContext';
import { TimeSeriesChart } from './TimeSeriesChart';
import { useRef, useEffect, useState } from 'react';
import { Sample } from '../types';

function formatBytes(b: number): string {
  if (b >= 1e9) return (b / 1e9).toFixed(1) + ' GB';
  if (b >= 1e6) return (b / 1e6).toFixed(1) + ' MB';
  if (b >= 1e3) return (b / 1e3).toFixed(1) + ' KB';
  return b + ' B';
}

export function IngestionMonitor() {
  const { state } = useDashboard();
  const stats = state.stats;
  const [rateHistory, setRateHistory] = useState<Sample[]>([]);
  const lastUpdateRef = useRef(0);

  useEffect(() => {
    if (!stats || !stats.ingestionRate) return;
    const now = Date.now();
    if (now - lastUpdateRef.current < 1000) return;
    lastUpdateRef.current = now;

    setRateHistory((prev) => {
      const next = [...prev, { timestamp: now, value: stats.ingestionRate }];
      return next.length > 120 ? next.slice(-120) : next;
    });
  }, [stats]);

  return (
    <div className="card">
      <h3 className="text-sm font-semibold text-gray-300 mb-3">Ingestion Monitor</h3>
      <div className="grid grid-cols-4 gap-4 mb-4">
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
      <TimeSeriesChart
        series={[{ label: 'Ingestion Rate', samples: rateHistory }]}
        height={140}
        showLegend={false}
        yLabel="samples/s"
        title="Ingestion Throughput"
      />
    </div>
  );
}
