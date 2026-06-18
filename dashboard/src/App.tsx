import { useDashboard } from './state/DashboardContext';
import { useMetricStream } from './hooks/useMetricStream';
import { QueryEditor } from './components/QueryEditor';
import { TimeSeriesChart } from './components/TimeSeriesChart';
import { MetricExplorer } from './components/MetricExplorer';
import { ClusterTopology } from './components/ClusterTopology';
import { IngestionMonitor } from './components/IngestionMonitor';
import { CompressionStats } from './components/CompressionStats';
import { LatencyHistogram } from './components/LatencyHistogram';
import { RetentionTimeline } from './components/RetentionTimeline';
import { LiveStream } from './components/LiveStream';
import { AlertsPanel } from './components/AlertsPanel';
import { ThemeToggle } from './components/ThemeToggle';
import { Panel } from './components/Panel';
import { Placeholder } from './components/Placeholder';
import { ConnectionStatus } from './types';
import { formatDuration, formatNumber } from './utils/format';
import { PANEL_BODY } from './utils/layout';

function connectionDotClass(status: ConnectionStatus): string {
  switch (status) {
    case 'connected':
      return 'bg-ok';
    case 'reconnecting':
      return 'bg-warn';
    default:
      return 'bg-muted';
  }
}

function connectionLabel(status: ConnectionStatus): string {
  switch (status) {
    case 'connected':
      return 'Live';
    case 'reconnecting':
      return 'Reconnecting…';
    default:
      return 'Connecting…';
  }
}

/**
 * A quiet banner that surfaces a dropped stream. The header lamp covers the
 * brief initial connect; this only appears once a live connection is actually
 * lost and the client is retrying with backoff.
 */
function ConnectionBanner({ status }: { status: ConnectionStatus }) {
  if (status !== 'reconnecting') return null;
  return (
    <div role="status" aria-live="polite" className="border-b border-warn/30 bg-warn/10 text-warn">
      <div className="max-w-[1600px] mx-auto px-4 py-1.5 flex items-center gap-2 text-xs">
        <span className="w-1.5 h-1.5 rounded-full bg-warn motion-safe:animate-pulse" aria-hidden="true" />
        Connection to the server was lost — reconnecting…
      </div>
    </div>
  );
}

function Dashboard() {
  const { state } = useDashboard();
  useMetricStream();

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

  const result = state.queryResult;
  const resultMeta =
    chartSeries.length > 0 && result ? (
      <>
        <span className="text-text">{result.data.length}</span> series
        {result.stats && (
          <>
            {' · '}
            <span className="text-text">{formatNumber(result.stats.samplesFetched)}</span> samples
            {' · '}
            <span className="text-text">{result.stats.executionMs}</span> ms
          </>
        )}
      </>
    ) : null;

  return (
    <div className="min-h-screen">
      {/* Header */}
      <header className="app-header">
        <div className="max-w-[1600px] mx-auto px-4 py-3 flex items-center justify-between gap-3 flex-wrap">
          <div className="flex items-center gap-3 min-w-0">
            {/* Transit-instrument mark: a graduated setting circle, the meridian
                line running clear through it, an inset horizon chord, and a star
                fixed on the meridian — the same crosshair the chart draws. */}
            <svg viewBox="0 0 32 32" className="w-7 h-7 text-accent" fill="none" stroke="currentColor" aria-hidden="true">
              <circle cx="16" cy="16" r="10.5" strokeWidth="1.6" />
              <line x1="16" y1="3" x2="16" y2="29" strokeWidth="1.6" strokeLinecap="round" />
              <line x1="9" y1="16" x2="23" y2="16" strokeWidth="1.4" strokeLinecap="round" />
              <circle cx="16" cy="10.5" r="1.9" fill="currentColor" stroke="none" />
            </svg>
            <div>
              <h1 className="text-base font-bold tracking-tight">Meridian</h1>
              <p className="text-2xs -mt-0.5 text-muted">Distributed Time-Series Database</p>
            </div>
          </div>

          <div className="flex items-center gap-3 sm:gap-4">
            {/* Connection status — a steady lamp when live, pulsing while transient.
                The text label carries the status, so it never relies on colour alone. */}
            <div role="status" aria-live="polite" className="flex items-center gap-1.5">
              <span
                aria-hidden="true"
                className={`w-2 h-2 rounded-full ${connectionDotClass(state.connectionStatus)} ${
                  state.connectionStatus === 'connected' ? '' : 'motion-safe:animate-pulse'
                }`}
              />
              <span className="text-xs text-muted">
                {connectionLabel(state.connectionStatus)}
              </span>
            </div>

            {/* Uptime */}
            {state.stats && Number.isFinite(state.stats.uptimeSeconds) && (
              <span className="text-xs text-muted tabular-nums">
                Up {formatDuration(state.stats.uptimeSeconds)}
              </span>
            )}

            <ThemeToggle />
          </div>
        </div>
      </header>

      <ConnectionBanner status={state.connectionStatus} />

      {/* Main content */}
      <main className="max-w-[1600px] mx-auto px-4 py-4 space-y-3 sm:space-y-4">
        {/* PRIMARY — the query and its result are the dominant surface */}
        <Panel tier="primary" className="relative z-10">
          <QueryEditor />
        </Panel>

        <Panel
          tier="primary"
          eyebrow="Query Result"
          title={
            <span className="font-mono text-sm font-medium text-text">
              {state.query || 'No query yet'}
            </span>
          }
          meta={resultMeta}
          bodyHeight={PANEL_BODY.signature}
        >
          {chartSeries.length > 0 ? (
            // Never blank a good chart on a later failure — keep the last result
            // and show a small running chip while the next query is in flight.
            <div className="relative flex-1 min-h-0">
              <TimeSeriesChart series={chartSeries} variant="instrument" />
              {state.queryLoading && (
                <div className="pointer-events-none absolute top-2 left-2 flex items-center gap-1.5 rounded border bg-surface px-2 py-1 text-2xs font-mono text-muted">
                  <span className="h-3 w-3 rounded-full border border-muted/30 border-t-accent motion-safe:animate-spin" />
                  Running…
                </div>
              )}
            </div>
          ) : state.queryLoading ? (
            <Placeholder kind="loading" title="Running query…" />
          ) : state.queryError ? (
            <Placeholder
              kind="error"
              title="That query didn't run"
              hint="See the message above the chart, then adjust the expression and run it again."
            />
          ) : state.queryResult ? (
            <Placeholder
              title="No series match this query."
              hint="Check the metric name and label filters, then run it again."
            />
          ) : (
            <Placeholder
              title="Run a query to plot a series."
              hint={
                <>
                  Enter a PromQL expression above — e.g.{' '}
                  <code className="font-mono text-text">rate(http_requests_total[5m])</code>.
                </>
              }
            />
          )}
        </Panel>

        {/* SECONDARY — live monitors */}
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-3 sm:gap-4">
          <IngestionMonitor />
          <ClusterTopology />
        </div>

        {/* Streaming anomaly detection (ADR-024) — out-of-band points in the live
            telemetry, raised and cleared in real time. */}
        <AlertsPanel />

        <LiveStream />

        {/* TERTIARY — dense readouts */}
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-3 sm:gap-4">
          <CompressionStats />
          <LatencyHistogram />
          <div className="sm:col-span-2">
            <RetentionTimeline />
          </div>
        </div>

        <MetricExplorer />
      </main>

      {/* Footer */}
      <footer className="border-t mt-8">
        <div className="max-w-[1600px] mx-auto px-4 py-3 text-xs text-muted">
          Meridian v0.2.0 — distributed time-series database
        </div>
      </footer>
    </div>
  );
}

export default function App() {
  return <Dashboard />;
}
