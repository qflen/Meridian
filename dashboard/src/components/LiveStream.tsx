import { useRef, useEffect } from 'react';
import { useDashboard } from '../state/DashboardContext';

export function LiveStream() {
  const { state } = useDashboard();
  const listRef = useRef<HTMLDivElement>(null);

  // Collect recent samples from all live metrics
  const entries: { key: string; ts: number; value: number }[] = [];
  state.liveMetrics.forEach((samples, key) => {
    const recent = samples.slice(-3);
    for (const s of recent) {
      entries.push({ key, ts: s.timestamp, value: s.value });
    }
  });
  entries.sort((a, b) => b.ts - a.ts);
  const display = entries.slice(0, 50);

  useEffect(() => {
    if (listRef.current) {
      listRef.current.scrollTop = 0;
    }
  }, [display.length]);

  const formatTs = (ts: number) => {
    const d = new Date(ts);
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
  };

  const formatVal = (v: number) => {
    if (Math.abs(v) >= 1e6) return (v / 1e6).toFixed(1) + 'M';
    if (Math.abs(v) >= 1e3) return (v / 1e3).toFixed(1) + 'K';
    return v.toFixed(v % 1 === 0 ? 0 : 2);
  };

  return (
    <div className="card">
      <div className="flex items-center justify-between mb-2">
        <h3 className="text-sm font-semibold text-gray-300">Live Stream</h3>
        <div className="flex items-center gap-2">
          {state.connected ? (
            <span className="flex items-center gap-1.5 text-xs text-green-400">
              <span className="w-1.5 h-1.5 rounded-full bg-green-400 animate-pulse" />
              Connected
            </span>
          ) : (
            <span className="flex items-center gap-1.5 text-xs text-gray-500">
              <span className="w-1.5 h-1.5 rounded-full bg-gray-600" />
              Disconnected
            </span>
          )}
        </div>
      </div>
      <div ref={listRef} className="h-52 overflow-y-auto font-mono text-xs space-y-px">
        {display.length === 0 && (
          <div className="text-gray-600 italic py-8 text-center text-xs">
            Waiting for live data...
          </div>
        )}
        {display.map((e, i) => (
          <div
            key={`${e.key}-${e.ts}-${i}`}
            className="flex items-center gap-2 px-2 py-1 rounded hover:bg-gray-800/50 transition-colors"
          >
            <span className="text-gray-600 w-16 shrink-0">{formatTs(e.ts)}</span>
            <span className="text-gray-400 truncate flex-1">{e.key}</span>
            <span className="text-meridian-400 font-medium w-16 text-right shrink-0">
              {formatVal(e.value)}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}
