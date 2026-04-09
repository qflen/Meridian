import { useState, useEffect } from 'react';
import { useDashboard } from '../state/DashboardContext';
import { TimeSeriesChart } from './TimeSeriesChart';

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
    // Fetch metric names (values of __name__ label)
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
    <div className="card">
      <div className="flex items-center justify-between mb-3">
        <h3 className="text-sm font-semibold text-gray-300">Metric Explorer</h3>
        <span className="text-xs text-gray-500">{metrics.length} metrics</span>
      </div>
      <input
        type="text"
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
        placeholder="Filter metrics..."
        className="input w-full mb-3 text-xs"
      />
      <div className="flex gap-3 h-64">
        <div className="w-1/2 overflow-y-auto space-y-0.5 pr-2">
          {filtered.length === 0 && (
            <div className="text-xs text-gray-600 italic py-4 text-center">
              {metrics.length === 0 ? 'Connect to server to browse metrics' : 'No matching metrics'}
            </div>
          )}
          {filtered.map((m) => (
            <button
              key={m.name}
              onClick={() => selectMetric(m.name)}
              className={`w-full text-left px-2 py-1.5 rounded text-xs font-mono transition-colors ${
                selected === m.name
                  ? 'bg-meridian-600/20 text-meridian-400'
                  : 'text-gray-400 hover:bg-gray-800 hover:text-gray-200'
              }`}
            >
              <span>{m.name}</span>
              <span className="ml-2 text-gray-600">
                {m.type === 'counter' ? 'CNT' : 'GAU'}
              </span>
            </button>
          ))}
        </div>
        <div className="w-1/2 flex items-center justify-center">
          {selected && liveData.length > 0 ? (
            <TimeSeriesChart series={liveData} height={240} showLegend={false} />
          ) : (
            <div className="text-xs text-gray-600 italic">
              {selected ? 'Waiting for data...' : 'Select a metric'}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
