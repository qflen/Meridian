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

  // Build chart series from query result
  const chartSeries =
    state.queryResult?.data?.map((ts, i) => {
      const labelStr = Object.entries(ts.labels)
        .filter(([k]) => k !== '__name__')
        .map(([k, v]) => `${k}="${v}"`)
        .join(', ');
      const name = ts.labels.__name__ || `series-${i}`;
      return {
        label: labelStr ? `${name}{${labelStr}}` : name,
        samples: ts.samples,
      };
    }) ?? [];

  return (
    <div className="min-h-screen bg-gray-950">
      {/* Header */}
      <header className="sticky top-0 z-40 border-b border-gray-800 bg-gray-950/90 backdrop-blur-sm">
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
              <h1 className="text-base font-bold text-white tracking-tight">Meridian</h1>
              <p className="text-[10px] text-gray-500 -mt-0.5">Distributed Time-Series Database</p>
            </div>
          </div>

          <div className="flex items-center gap-4">
            {/* Frame metrics */}
            <div className="hidden md:flex items-center gap-3 text-xs text-gray-500">
              <span>{frameMetrics.fps} fps</span>
              <span>{frameMetrics.frameTime}ms</span>
              {frameMetrics.droppedFrames > 0 && (
                <span className="text-yellow-500">{frameMetrics.droppedFrames} dropped</span>
              )}
            </div>

            {/* Connection status */}
            <div className="flex items-center gap-1.5">
              <span
                className={`w-2 h-2 rounded-full ${
                  state.connected ? 'bg-green-400 animate-pulse' : 'bg-gray-600'
                }`}
              />
              <span className="text-xs text-gray-400">
                {state.connected ? 'Live' : 'Offline'}
              </span>
            </div>

            {/* Uptime */}
            {state.stats && (
              <span className="text-xs text-gray-500">
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
        <div className="card">
          <QueryEditor />
        </div>

        {/* Query result chart */}
        {chartSeries.length > 0 && (
          <div className="card">
            <div className="flex items-center justify-between mb-2">
              <h3 className="text-sm font-semibold text-gray-300">Query Result</h3>
              <span className="text-xs text-gray-500">
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

        {/* Middle row: Cluster + Live + Histogram */}
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          <ClusterTopology />
          <LiveStream />
          <LatencyHistogram />
        </div>

        {/* Bottom row: Explorer + Retention */}
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
          <MetricExplorer />
          <RetentionTimeline />
        </div>
      </main>

      {/* Footer */}
      <footer className="border-t border-gray-800 mt-8">
        <div className="max-w-[1600px] mx-auto px-4 py-3 flex items-center justify-between text-xs text-gray-600">
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
