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
    <div className="card flex flex-col h-[294px]">
      <div className="flex items-center justify-between mb-2">
        <h3 className="text-sm font-semibold" style={{ color: 'rgb(var(--color-text))' }}>Live Stream</h3>
        <div className="flex items-center gap-2">
          {state.connected ? (
            <span className="flex items-center gap-1.5 text-xs" style={{ color: 'rgb(var(--color-success))' }}>
              <span className="w-1.5 h-1.5 rounded-full animate-pulse" style={{ backgroundColor: 'rgb(var(--color-success))' }} />
              Connected
            </span>
          ) : (
            <span className="flex items-center gap-1.5 text-xs" style={{ color: 'rgb(var(--color-text-muted))' }}>
              <span className="w-1.5 h-1.5 rounded-full" style={{ backgroundColor: 'rgb(var(--color-text-muted))' }} />
              Disconnected
            </span>
          )}
        </div>
      </div>
      <div ref={listRef} className="flex-1 min-h-0 overflow-y-auto font-mono text-xs space-y-px">
        {display.length === 0 && (
          <div className="italic py-8 text-center text-xs" style={{ color: 'rgb(var(--color-text-muted))' }}>
            Waiting for live data...
          </div>
        )}
        {display.map((e, i) => (
          <div
            key={`${e.key}-${e.ts}-${i}`}
            className="flex items-center gap-2 px-2 py-1 rounded transition-colors"
            style={{ cursor: 'default' }}
            onMouseEnter={(el) => el.currentTarget.style.background = 'rgb(var(--color-text) / 0.06)'}
            onMouseLeave={(el) => el.currentTarget.style.background = 'transparent'}
          >
            <span className="w-24 shrink-0 whitespace-nowrap" style={{ color: 'rgb(var(--color-text-muted))' }}>{formatTs(e.ts)}</span>
            <span className="flex-1 break-all" style={{ color: 'rgb(var(--color-text))' }}>{e.key}</span>
            <span className="text-meridian-400 font-medium w-16 text-right shrink-0">
              {formatVal(e.value)}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}
