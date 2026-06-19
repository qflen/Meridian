import { useEffect } from 'react';
import { useDashboard } from '../state/DashboardContext';
import { Panel } from './Panel';
import { Placeholder } from './Placeholder';
import { coerceAnomaly } from '../utils/wsMessage';
import { formatNumber, formatTime } from '../utils/format';
import { PANEL_BODY } from '../utils/layout';
import { Anomaly, AnomalyModel } from '../types';

// Human labels for the active detector model (ADR-028), shown as a small readout in
// the panel header. Empty model (detector disabled or not yet seeded) shows nothing.
const MODEL_LABEL: Record<Exclude<AnomalyModel, ''>, string> = {
  ewma: 'EWMA',
  holt_winters: 'Holt-Winters',
};

function coerceModel(raw: unknown): AnomalyModel {
  return raw === 'ewma' || raw === 'holt_winters' ? raw : '';
}

/**
 * Anomalies — the alerts strip for the streaming detector (ADR-024). One row per
 * series carrying its latest transition: a firing row in the warn/crit status
 * token, dimmed to neutral once it clears. Severity is always carried by a text
 * label as well as colour, so it never relies on hue alone. Figures are tabular
 * mono so the value/baseline/score columns align like a readout.
 */

function statusToken(a: Anomaly): { dot: string; text: string } {
  if (a.state === 'resolved') return { dot: 'bg-muted', text: 'text-muted' };
  return a.severity === 'crit'
    ? { dot: 'bg-crit', text: 'text-crit' }
    : { dot: 'bg-warn', text: 'text-warn' };
}

// A z-score readout: one decimal, but collapse to a whole number once large so the
// column stays narrow.
function formatScore(score: number): string {
  return score >= 100 ? Math.round(score).toString() : score.toFixed(1);
}

export function AlertsPanel() {
  const { state, dispatch } = useDashboard();

  // Seed the recent buffer once on mount so a late-joining client shows history
  // immediately; live frames then keep the strip current.
  useEffect(() => {
    let cancelled = false;
    fetch('/api/v1/anomalies')
      .then((r) => (r.ok ? r.json() : null))
      .then((data) => {
        if (cancelled || !data || !Array.isArray(data.anomalies)) return;
        const seeded = (data.anomalies as unknown[])
          .map(coerceAnomaly)
          .filter((a): a is Anomaly => a !== null);
        // Seed even when empty so the active model (from the same payload) is recorded.
        dispatch({ type: 'SEED_ANOMALIES', anomalies: seeded, model: coerceModel(data.model) });
      })
      .catch(() => {
        /* a missing endpoint just means no seed; live frames still populate */
      });
    return () => {
      cancelled = true;
    };
  }, [dispatch]);

  const { anomalies, anomalyModel } = state;
  const activeCount = anomalies.reduce((n, a) => (a.state === 'firing' ? n + 1 : n), 0);

  // The active detector model, as a restrained readout, separated from the live
  // status by a hairline rule.
  const modelToken = anomalyModel ? (
    <span
      className="text-2xs uppercase tracking-wider text-muted/70"
      title={`Detector model: ${MODEL_LABEL[anomalyModel]}`}
    >
      {MODEL_LABEL[anomalyModel]}
    </span>
  ) : null;

  const status =
    activeCount > 0 ? (
      <span className="flex items-center gap-1.5 text-crit">
        <span className="w-1.5 h-1.5 rounded-full bg-crit" />
        {activeCount} active
      </span>
    ) : anomalies.length > 0 ? (
      <span className="flex items-center gap-1.5 text-ok">
        <span className="w-1.5 h-1.5 rounded-full bg-ok" />
        all clear
      </span>
    ) : null;

  const meta =
    modelToken || status ? (
      <span className="flex items-center gap-2.5">
        {modelToken}
        {modelToken && status && <span className="h-3 w-px bg-text/10" aria-hidden="true" />}
        {status}
      </span>
    ) : null;

  return (
    <Panel tier="secondary" title="Anomalies" meta={meta} bodyHeight={PANEL_BODY.ticker}>
      <div className="flex-1 min-h-0 overflow-y-auto space-y-px" aria-label="Recent anomalies">
        {anomalies.length === 0 ? (
          <Placeholder
            title="No anomalies detected."
            hint="Out-of-band points in the live telemetry are flagged here as they happen."
          />
        ) : (
          anomalies.map((a) => <AnomalyRow key={a.series} a={a} />)
        )}
      </div>
    </Panel>
  );
}

// Fixed column widths: value / vs / baseline / score / state line up down the
// strip no matter how long any one figure is.
function AnomalyRow({ a }: { a: Anomaly }) {
  const token = statusToken(a);
  const resolved = a.state === 'resolved';
  return (
    <div className={`readout-row ${resolved ? 'opacity-60' : ''}`}>
      <span className={`w-1.5 h-1.5 shrink-0 rounded-full ${token.dot}`} aria-hidden="true" />
      <span className="w-16 shrink-0 text-muted">{formatTime(a.timestamp)}</span>
      <span className="flex-1 min-w-0 truncate text-text" title={a.series}>
        {a.series}
      </span>
      <span className="hidden sm:flex shrink-0 items-baseline">
        <span className="w-16 text-right text-text">{formatNumber(a.value)}</span>
        <span className="w-8 text-center text-muted/60">vs</span>
        <span className="w-16 text-right text-muted">{formatNumber(a.baseline)}</span>
      </span>
      <span className={`w-14 shrink-0 text-right ${token.text}`}>{formatScore(a.score)}σ</span>
      <span className={`w-14 shrink-0 text-right text-2xs uppercase tracking-wide ${token.text}`}>
        {resolved ? 'cleared' : a.severity}
      </span>
    </div>
  );
}
