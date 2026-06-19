import { useRef, useEffect, useCallback, useState } from 'react';
import { Panel } from './Panel';
import { Placeholder } from './Placeholder';
import { getCanvasColors } from '../utils/canvasColors';
import { canvasFont } from '../utils/canvasFont';
import { formatNumber } from '../utils/format';
import { niceTicks, formatTick } from '../utils/ticks';
import { PANEL_BODY } from '../utils/layout';

interface Bucket {
  le: string;
  count: number;
}

const EMPTY_BUCKETS: Bucket[] = ['1ms', '5ms', '10ms', '25ms', '50ms', '100ms', '250ms', '500ms', '1s'].map(
  (le) => ({ le, count: 0 }),
);

export function LatencyHistogram() {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const [buckets, setBuckets] = useState<Bucket[]>([]);

  // Fetch real query latency data from the gateway
  useEffect(() => {
    const fetchData = () => {
      fetch('/api/v1/query_latency')
        .then((r) => r.json())
        .then((data) => {
          if (Array.isArray(data) && data.length > 0) {
            const bkts: Bucket[] = data
              .filter((b: { le?: string; count?: number }) => b.le && typeof b.count === 'number')
              .map((b: { le: string; count: number }) => ({
                le: b.le,
                count: b.count,
              }));
            if (bkts.length > 0) setBuckets(bkts);
          }
        })
        .catch(() => {});
    };

    fetchData();
    const interval = setInterval(fetchData, 5000);
    return () => clearInterval(interval);
  }, []);

  // Axis scaffolding when no data
  const displayBuckets = buckets.length > 0 && buckets.some((b) => b.count > 0) ? buckets : EMPTY_BUCKETS;
  const isEmpty = displayBuckets.every((b) => b.count === 0);
  const total = displayBuckets.reduce((n, b) => n + b.count, 0);

  const render = useCallback(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const ctx = canvas.getContext('2d');
    if (!ctx) return;

    const dpr = window.devicePixelRatio || 1;
    const w = canvas.clientWidth;
    const h = canvas.clientHeight;
    canvas.width = w * dpr;
    canvas.height = h * dpr;
    ctx.scale(dpr, dpr);
    ctx.clearRect(0, 0, w, h);

    const colors = getCanvasColors(canvas);
    const maxCount = Math.max(...displayBuckets.map((b) => b.count), 1);
    const yAxis = niceTicks(0, maxCount, Math.max(2, Math.floor((h - 32) / 28)), { integer: true });
    const yLabels = yAxis.ticks.map((v) => formatTick(v, yAxis.step));

    // The left gutter fits the widest count label.
    ctx.font = canvasFont(9);
    const widest = yLabels.reduce((m, l) => Math.max(m, ctx.measureText(l).width), 0);
    const pad = { top: 8, right: 8, bottom: 22, left: Math.max(24, Math.ceil(widest) + 10) };
    const plotW = w - pad.left - pad.right;
    const plotH = h - pad.top - pad.bottom;
    const slotW = plotW / displayBuckets.length;
    const barW = Math.max(4, slotW - 4);
    const plotBottom = pad.top + plotH;

    // Bucket labels: skip every n-th when they would run into each other, so
    // the axis stays legible at any panel width.
    const labelW = displayBuckets.reduce((m, b) => Math.max(m, ctx.measureText(b.le).width), 0);
    const every = Math.max(1, Math.ceil((labelW + 6) / slotW));
    ctx.fillStyle = colors.textMuted;
    ctx.textAlign = 'center';
    ctx.textBaseline = 'alphabetic';
    displayBuckets.forEach((bucket, i) => {
      if (i % every !== 0) return;
      const x = pad.left + i * slotW + slotW / 2;
      ctx.fillText(bucket.le, x, plotBottom + 14);
    });

    // Baseline
    ctx.strokeStyle = colors.gridStrong;
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(pad.left, plotBottom + 0.5);
    ctx.lineTo(pad.left + plotW, plotBottom + 0.5);
    ctx.stroke();

    // When every bucket is empty, draw only the axis scaffolding; the shared
    // Placeholder is overlaid in the DOM, so all empty states match.
    if (isEmpty) return;

    // Count gridlines + labels on integer nice ticks (never a repeated value).
    ctx.font = canvasFont(9);
    ctx.textAlign = 'right';
    ctx.textBaseline = 'middle';
    yAxis.ticks.forEach((v, i) => {
      const y = plotBottom - (v / yAxis.max) * plotH;
      if (v > 0) {
        ctx.strokeStyle = colors.gridColor;
        ctx.lineWidth = 0.5;
        ctx.beginPath();
        ctx.moveTo(pad.left, y);
        ctx.lineTo(pad.left + plotW, y);
        ctx.stroke();
      }
      ctx.fillStyle = colors.textMuted;
      ctx.fillText(yLabels[i], pad.left - 6, y);
    });

    displayBuckets.forEach((bucket, i) => {
      if (bucket.count === 0) return;
      const barH = (bucket.count / yAxis.max) * plotH;
      const x = pad.left + i * slotW + (slotW - barW) / 2;
      const y = plotBottom - barH;
      ctx.fillStyle = colors.accent;
      ctx.fillRect(x, y, barW, barH);
    });
  }, [displayBuckets, isEmpty]);

  useEffect(() => {
    render();
  }, [render]);

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;
    const ro = new ResizeObserver(() => render());
    ro.observe(container);
    return () => ro.disconnect();
  }, [render]);

  const meta = isEmpty ? null : `${formatNumber(total)} ${total === 1 ? 'query' : 'queries'}`;

  return (
    <Panel tier="tertiary" title="Query Latency" meta={meta} bodyHeight={PANEL_BODY.compact}>
      <div ref={containerRef} className="relative flex-1 min-h-0">
        <canvas ref={canvasRef} className="block w-full h-full" />
        {isEmpty && (
          <div className="pointer-events-none absolute inset-0">
            <Placeholder
              title="No query latencies recorded yet."
              hint="Run a query and the distribution fills in."
            />
          </div>
        )}
      </div>
    </Panel>
  );
}
