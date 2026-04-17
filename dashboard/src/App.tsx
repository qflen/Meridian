import { useDashboard } from './state/DashboardContext';
import { useMetricStream } from './hooks/useMetricStream';
import { useFrameMetrics } from './hooks/useFrameMetrics';
import { QueryEditor } from './components/QueryEditor';
import { TimeSeriesChart } from './components/TimeSeriesChart';
import { MetricExplorer } from './components/MetricExplorer';
import { ClusterTopology } from './components/ClusterTopology';
import { IngestionMonitor } from './components/IngestionMonitor';
import { CompressionStats } from './components/CompressionStats';
import { LatencyHistogram } from './components/LatencyHistogram';
import { RetentionTimeline } from './components/RetentionTimeline';
import { LiveStream } from './components/LiveStream';
import { ThemeToggle } from './components/ThemeToggle';

function Dashboard() {
  const { state } = useDashboard();
  useMetricStream();
  const frameMetrics = useFrameMetrics();

  // Build chart series from query result — use short legend labels
  const chartSeries = (() => {
    const data = state.queryResult?.data ?? [];
    if (data.length === 0) return [];

    // Find which label keys differ across series (skip __name__)
    const allKeys = new Set<string>();
    for (const ts of data) {
      for (const k of Object.keys(ts.labels)) {
        if (k !== '__name__') allKeys.add(k);
      }
    }
    const varyingKeys = [...allKeys].filter((k) => {
      const vals = new Set(data.map((ts) => ts.labels[k] ?? ''));
      return vals.size > 1;
    });

    return data.map((ts, i) => {
      let label: string;
      if (varyingKeys.length > 0) {
        // Show only the labels that differ between series
        label = varyingKeys
          .map((k) => ts.labels[k] ?? '')
          .filter(Boolean)
          .join(', ');
      } else {
        // All labels are the same — just show the metric name
        label = ts.labels.__name__ || `series-${i}`;
      }
      return { label: label || `series-${i}`, samples: ts.samples };
    });
  })();

  return (
    <div className="min-h-screen">
      {/* Header */}
      <header className="app-header">
        <div className="max-w-[1600px] mx-auto px-4 py-3 flex items-center justify-between">
          <div className="flex items-center gap-3">
            <svg viewBox="0 0 32 32" className="w-7 h-7">
              <circle cx="16" cy="16" r="14" fill="#4c6ef5" />
              <path
                d="M8 20 L14 12 L18 16 L24 8"
                stroke="white"
                strokeWidth="2.5"
                fill="none"
                strokeLinecap="round"
              />
            </svg>
            <div>
              <h1 className="text-base font-bold tracking-tight">Meridian</h1>
              <p className="text-[10px] -mt-0.5" style={{ color: 'rgb(var(--color-text-muted))' }}>Distributed Time-Series Database</p>
            </div>
          </div>

          <div className="flex items-center gap-4">
            {/* Frame metrics */}
            <div className="hidden md:flex items-center gap-3 text-xs" style={{ color: 'rgb(var(--color-text-muted))' }}>
              <span>{frameMetrics.fps} fps</span>
              <span>{frameMetrics.frameTime}ms</span>
              {frameMetrics.droppedFrames > 0 && (
                <span style={{ color: 'rgb(var(--color-warning))' }}>{frameMetrics.droppedFrames} dropped</span>
              )}
            </div>

            {/* Connection status */}
            <div className="flex items-center gap-1.5">
              <span
                className={`w-2 h-2 rounded-full ${
                  state.connected ? 'animate-pulse' : ''
                }`}
                style={{ backgroundColor: state.connected ? 'rgb(var(--color-success))' : 'rgb(var(--color-text-muted))' }}
              />
              <span className="text-xs" style={{ color: 'rgb(var(--color-text-muted))' }}>
                {state.connected ? 'Live' : 'Offline'}
              </span>
            </div>

            {/* Uptime */}
            {state.stats && (
              <span className="text-xs" style={{ color: 'rgb(var(--color-text-muted))' }}>
                Up {Math.floor(state.stats.uptimeSeconds / 60)}m
              </span>
            )}

            <ThemeToggle />
          </div>
        </div>
      </header>

      {/* Main content */}
      <main className="max-w-[1600px] mx-auto px-4 py-4 space-y-4">
        {/* Query bar */}
        <div className="card relative z-10">
          <QueryEditor />
        </div>

        {/* Query result chart */}
        {chartSeries.length > 0 && (
          <div className="card">
            <div className="flex items-center justify-between mb-2">
              <h3 className="text-sm font-semibold" style={{ color: 'rgb(var(--color-text))' }}>Query Result</h3>
              <span className="text-xs" style={{ color: 'rgb(var(--color-text-muted))' }}>
                {state.queryResult?.data?.length ?? 0} series
                {state.queryResult?.stats &&
                  ` | ${state.queryResult.stats.samplesFetched} samples in ${state.queryResult.stats.executionMs}ms`}
              </span>
            </div>
            <TimeSeriesChart series={chartSeries} height={280} />
          </div>
        )}

        {/* Top row: Ingestion + Compression + Latency */}
        <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">
          <div className="lg:col-span-2">
            <IngestionMonitor />
          </div>
          <CompressionStats />
        </div>

        {/* Middle row: Live (wide) + Cluster + Histogram */}
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4">
          <div className="lg:col-span-2">
            <LiveStream />
          </div>
          <ClusterTopology />
          <LatencyHistogram />
        </div>

        {/* Bottom row: Explorer + Retention */}
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
          <MetricExplorer />
          <RetentionTimeline />
        </div>
      </main>

      {/* Footer */}
      <footer className="border-t mt-8" style={{ borderColor: 'rgb(var(--color-border))' }}>
        <div className="max-w-[1600px] mx-auto px-4 py-3 flex items-center justify-between text-xs" style={{ color: 'rgb(var(--color-text-muted))' }}>
          <span>Meridian TSDB v0.1.0</span>
          <span>Canvas-rendered at 60fps | Zero chart dependencies</span>
        </div>
      </footer>
    </div>
  );
}

export default function App() {
  return <Dashboard />;
}
