import { useDashboard } from '../state/DashboardContext';
import { Panel } from './Panel';
import { compressionRatio } from '../utils/compression';
import { formatBytes } from '../utils/format';
import { PANEL_BODY } from '../utils/layout';

/**
 * Gorilla compression as a direct size comparison: the raw footprint is the
 * full track, the compressed footprint the accent fill, and the ratio between
 * them is the headline figure. No gauge; the two bars are the measurement.
 */
export function CompressionStats() {
  const { state } = useDashboard();
  const stats = state.stats;
  const raw = stats?.rawBytes ?? 0;
  const compressed = stats?.compressedBytes ?? 0;
  const ratio = stats ? compressionRatio(raw, compressed) : 0;
  const compressedPct = raw > 0 ? Math.min(100, (compressed / raw) * 100) : 0;

  return (
    <Panel tier="tertiary" title="Gorilla Compression" bodyHeight={PANEL_BODY.compact}>
      <div className="flex-1 min-h-0 flex flex-col justify-between">
        <div>
          <div className="stat-value">{ratio > 0 ? `${ratio.toFixed(1)}x` : '--'}</div>
          <div className="stat-label">compression ratio</div>
        </div>
        <div className="space-y-3">
          <SizeBar label="raw" value={stats ? formatBytes(raw) : '--'} pct={raw > 0 ? 100 : 0} fill="bg-muted/50" />
          <SizeBar
            label="compressed"
            value={stats ? formatBytes(compressed) : '--'}
            pct={compressedPct}
            fill="bg-accent"
            valueClass="text-accent"
          />
        </div>
      </div>
    </Panel>
  );
}

function SizeBar({
  label,
  value,
  pct,
  fill,
  valueClass = 'text-text',
}: {
  label: string;
  value: string;
  pct: number;
  fill: string;
  valueClass?: string;
}) {
  return (
    <div>
      <div className="flex items-baseline justify-between gap-2 mb-1.5">
        <span className="stat-label">{label}</span>
        <span className={`font-mono text-2xs tabular-nums ${valueClass}`}>{value}</span>
      </div>
      <div className="track" role="meter" aria-label={`${label} size`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pct)}>
        <div className={`h-full ${fill}`} style={{ width: `${pct}%` }} />
      </div>
    </div>
  );
}
