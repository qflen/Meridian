import { useState, useEffect } from 'react';
import { useDashboard } from '../state/DashboardContext';
import { TimeSeriesChart } from './TimeSeriesChart';
import { Panel } from './Panel';
import { Placeholder } from './Placeholder';
import { PANEL_BODY } from '../utils/layout';

interface MetricMeta {
  name: string;
  type: string;
  labels: string[];
  seriesCount: number;
}

export function MetricExplorer() {
  const { dispatch, state } = useDashboard();
  const [metrics, setMetrics] = useState<MetricMeta[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [filter, setFilter] = useState('');

  useEffect(() => {
    const fetchMetrics = () => {
      fetch('/api/v1/label/__name__/values')
        .then((r) => r.json())
        .then((resp) => {
          const names = resp?.data ?? resp;
          if (Array.isArray(names)) {
            const metas: MetricMeta[] = names.map((name: string) => ({
              name,
              type: name.endsWith('_total') ? 'counter' : name.endsWith('_bytes') ? 'gauge' : 'gauge',
              labels: [],
              seriesCount: 0,
            }));
            setMetrics(metas);
          }
        })
        .catch(() => {});
    };

    fetchMetrics();
    const interval = setInterval(fetchMetrics, 10000);
    return () => clearInterval(interval);
  }, []);

  const filtered = metrics.filter((m) =>
    m.name.toLowerCase().includes(filter.toLowerCase()),
  );

  const selectMetric = (name: string) => {
    setSelected(name);
    dispatch({ type: 'SET_QUERY', query: name });
  };

  // Build chart data from live metrics
  const liveData = selected
    ? Array.from(state.liveMetrics.entries())
        .filter(([key]) => key.includes(selected))
        .slice(0, 5)
        .map(([key, samples]) => ({
          label: key,
          samples,
        }))
    : [];

  return (
    <Panel
      tier="tertiary"
      title="Metric Explorer"
      meta={`${metrics.length} metrics`}
      bodyHeight={PANEL_BODY.explorer}
    >
      <input
        type="text"
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
        placeholder="Filter metrics"
        aria-label="Filter metrics"
        className="input w-full mb-3 text-xs"
      />
      <div className="flex gap-4 flex-1 min-h-0">
        <div className="w-1/2 overflow-y-auto space-y-px pr-1">
          {filtered.length === 0 &&
            (metrics.length === 0 ? (
              <Placeholder title="No metrics to browse yet." hint="They appear once the server starts reporting." />
            ) : (
              <Placeholder title="No metrics match this filter." hint="Clear the filter to see them all." />
            ))}
          {filtered.map((m) => (
            <button
              key={m.name}
              type="button"
              onClick={() => selectMetric(m.name)}
              aria-pressed={selected === m.name}
              className={`w-full flex items-center justify-between gap-3 px-2 py-1 rounded text-xs font-mono transition-colors ${
                selected === m.name ? 'bg-accent/15 text-accent' : 'text-text hover:bg-text/5'
              }`}
            >
              <span className="truncate">{m.name}</span>
              <span className="shrink-0 text-2xs text-muted">{m.type}</span>
            </button>
          ))}
        </div>
        <div className="w-1/2 min-h-0 border-l pl-4">
          {selected && liveData.length > 0 ? (
            <div className="w-full h-full">
              <TimeSeriesChart series={liveData} showLegend={false} />
            </div>
          ) : selected ? (
            <Placeholder title="Waiting for samples…" hint="Live values for this metric will plot here." />
          ) : (
            <Placeholder title="Select a metric to preview." hint="Pick one on the left to plot its live values." />
          )}
        </div>
      </div>
    </Panel>
  );
}
