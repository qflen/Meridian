import { useRef, useEffect } from 'react';
import { useDashboard } from '../state/DashboardContext';
import { LiveEntry, liveRowKey } from '../utils/liveStream';
import { formatNumber, formatTime } from '../utils/format';

export function LiveStream() {
  const { state } = useDashboard();
  const listRef = useRef<HTMLDivElement>(null);

  // Collect recent samples from all live metrics
  const entries: LiveEntry[] = [];
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

  return (
    <div className="card flex flex-col h-[294px]">
      <div className="flex items-center justify-between mb-2">
        <h3 className="text-sm font-semibold">Live Stream</h3>
        <div className="flex items-center gap-2">
          {state.connectionStatus === 'connected' ? (
            <span className="flex items-center gap-1.5 text-xs text-ok">
              <span className="w-1.5 h-1.5 rounded-full bg-ok" />
              Connected
            </span>
          ) : (
            <span className="flex items-center gap-1.5 text-xs text-muted">
              <span className="w-1.5 h-1.5 rounded-full bg-muted animate-pulse" />
              {state.connectionStatus === 'reconnecting' ? 'Reconnecting…' : 'Connecting…'}
            </span>
          )}
        </div>
      </div>
      <div ref={listRef} className="flex-1 min-h-0 overflow-y-auto font-mono text-xs tabular-nums space-y-px">
        {display.length === 0 && (
          <div className="italic py-8 text-center text-xs text-muted">
            Waiting for live data...
          </div>
        )}
        {display.map((e) => (
          <div
            key={liveRowKey(e)}
            className="flex items-center gap-2 px-2 py-1 rounded cursor-default transition-colors hover:bg-text/5"
          >
            <span className="w-24 shrink-0 whitespace-nowrap text-muted">{formatTime(e.ts)}</span>
            <span className="flex-1 break-all text-text">{e.key}</span>
            <span className="text-accent font-medium w-16 text-right shrink-0">
              {formatNumber(e.value)}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}
