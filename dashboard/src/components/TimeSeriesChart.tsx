import { useRef, useEffect, useCallback, useState } from 'react';
import { Sample } from '../types';
import { getCanvasColors } from '../utils/canvasColors';
import { canvasFont } from '../utils/canvasFont';
import { formatNumber, formatTime } from '../utils/format';
import { CATEGORICAL } from '../utils/chartPalette';
import { nearestSampleIndex, nearestSample } from '../utils/nearestSample';
import { niceTicks, formatTick } from '../utils/ticks';

interface SeriesData {
  label: string;
  samples: Sample[];
  color?: string;
}

interface Props {
  series: SeriesData[];
  showGrid?: boolean;
  showLegend?: boolean;
  animated?: boolean;
  /**
   * `instrument` is the signature treatment — a finer graticule, instrument
   * tick marks, and a cursor crosshair with a live readout. Reserved for the
   * one primary chart; every other chart stays `plain` and quiet.
   */
  variant?: 'plain' | 'instrument';
  /**
   * `auto` fits the y-axis to the data; `zero` anchors it at zero so a rate or
   * count reads against its true baseline instead of a magnified band around a
   * near-constant value.
   */
  baseline?: 'auto' | 'zero';
}

/** Plot geometry shared between the base render and the crosshair pass. */
interface Geom {
  pad: { top: number; right: number; bottom: number; left: number };
  plotW: number;
  plotH: number;
  minT: number;
  tRange: number;
  minV: number;
  maxV: number;
}

interface ReadoutPoint {
  label: string;
  color: string;
  value: number;
}

const prefersReducedMotion = () =>
  typeof window !== 'undefined' &&
  typeof window.matchMedia === 'function' &&
  window.matchMedia('(prefers-reduced-motion: reduce)').matches;

const MAX_READOUT_ROWS = 10;
const LEGEND_ROW_H = 16;
const LEGEND_SWATCH_W = 12;
const MAX_LEGEND_ITEMS = 12;

export function TimeSeriesChart({
  series,
  showGrid = true,
  showLegend = true,
  animated = true,
  variant = 'plain',
  baseline = 'auto',
}: Props) {
  const instrument = variant === 'instrument';
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const sweepRafRef = useRef(0);
  const progressRef = useRef(0);
  const hasAnimatedRef = useRef(false);

  // Crosshair plumbing — all refs so the pointer handlers stay stable and read
  // the latest data without re-subscribing on every data tick.
  const geomRef = useRef<Geom | null>(null);
  const paletteRef = useRef<string[]>([]);
  const seriesRef = useRef<SeriesData[]>(series);
  seriesRef.current = series;
  const cursorRef = useRef<{ t: number } | null>(null);
  const pointerXRef = useRef(0);
  const hoverRafRef = useRef(0);
  const renderRef = useRef<() => void>(() => {});
  const [readout, setReadout] = useState<{ t: number; points: ReadoutPoint[] } | null>(null);

  const render = useCallback(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const ctx = canvas.getContext('2d');
    if (!ctx) return;

    const dpr = window.devicePixelRatio || 1;
    const w = canvas.clientWidth;
    const h = canvas.clientHeight;
    const pxW = Math.round(w * dpr);
    const pxH = Math.round(h * dpr);
    // Only resize the backing store when the device-pixel dimensions actually
    // change — assigning canvas.width every frame reallocates the backing store.
    if (canvas.width !== pxW || canvas.height !== pxH) {
      canvas.width = pxW;
      canvas.height = pxH;
    }
    // Absolute transform (not the cumulative ctx.scale): since the backing store
    // is no longer cleared each frame, a cumulative scale would compound.
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);

    const colors = getCanvasColors(canvas);
    // The primary/live trace takes the single accent; extra series fall back to
    // the restrained categorical secondaries.
    const palette = [colors.accent, ...CATEGORICAL];
    paletteRef.current = palette;

    // Compute data bounds
    let minT = Infinity, maxT = -Infinity, minV = Infinity, maxV = -Infinity;
    for (const s of series) {
      for (const p of s.samples) {
        if (p.timestamp < minT) minT = p.timestamp;
        if (p.timestamp > maxT) maxT = p.timestamp;
        if (p.value < minV) minV = p.value;
        if (p.value > maxV) maxV = p.value;
      }
    }

    if (!isFinite(minT)) {
      // No samples — the canvas is left blank; callers render a shared
      // Placeholder over the empty body so every empty state reads alike.
      geomRef.current = null;
      return;
    }

    // Y range: breathe 5% around the data, then snap to nice ticks. A zero
    // baseline pins the floor at 0 whenever the data is non-negative.
    const spread = maxV - minV || Math.abs(maxV) * 0.1 || 1;
    let lo = minV - spread * 0.05;
    let hi = maxV + spread * 0.05;
    if (baseline === 'zero' && minV >= 0) lo = 0;
    if (minV >= 0 && lo < 0) lo = 0;

    // Legend rows are laid out below the time axis; the gutter grows per row.
    const legendItems = showLegend ? Math.min(series.length, MAX_LEGEND_ITEMS) : 0;
    const legendRows = legendItems > 0 ? Math.ceil(legendItems / 3) : 0;
    const pad = {
      top: 12,
      right: 12,
      bottom: 22 + (legendRows > 0 ? 6 + legendRows * LEGEND_ROW_H : 0),
      left: 36,
    };

    // Ticks: about one per 40px vertically, one per 110px horizontally.
    const provisionalPlotH = h - pad.top - pad.bottom;
    const yCount = Math.max(2, Math.min(6, Math.floor(provisionalPlotH / 40)));
    const yAxis = niceTicks(lo, hi, yCount);
    minV = yAxis.min;
    maxV = yAxis.max;

    // Size the left gutter to the widest tick label so labels never collide
    // with the plot frame or each other.
    ctx.font = canvasFont(10);
    const yLabels = yAxis.ticks.map((v) => formatTick(v, yAxis.step));
    const widest = yLabels.reduce((m, l) => Math.max(m, ctx.measureText(l).width), 0);
    pad.left = Math.max(pad.left, Math.ceil(widest) + (instrument ? 14 : 12));

    const plotW = w - pad.left - pad.right;
    const plotH = h - pad.top - pad.bottom;
    const tRange = maxT - minT || 1;

    geomRef.current = { pad, plotW, plotH, minT, tRange, minV, maxV };

    const toX = (t: number) => pad.left + ((t - minT) / tRange) * plotW;
    const toY = (v: number) => pad.top + plotH - ((v - minV) / (maxV - minV)) * plotH;
    const plotBottom = pad.top + plotH;

    // Grid + axes
    if (showGrid) {
      const xTicks = Math.max(2, Math.min(6, Math.floor(plotW / 110)));

      // Minor graticule subdivisions (instrument only): one faint line between
      // each pair of majors, both axes.
      if (instrument) {
        ctx.strokeStyle = colors.gridFaint;
        ctx.lineWidth = 0.5;
        for (let i = 0; i < yAxis.ticks.length - 1; i++) {
          const y = toY((yAxis.ticks[i] + yAxis.ticks[i + 1]) / 2);
          ctx.beginPath();
          ctx.moveTo(pad.left, y);
          ctx.lineTo(pad.left + plotW, y);
          ctx.stroke();
        }
        for (let i = 0; i < xTicks; i++) {
          const x = toX(minT + ((i + 0.5) / xTicks) * tRange);
          ctx.beginPath();
          ctx.moveTo(x, pad.top);
          ctx.lineTo(x, plotBottom);
          ctx.stroke();
        }
      }

      // Major lines + tabular-mono labels + instrument tick marks
      ctx.lineWidth = 0.5;
      yAxis.ticks.forEach((v, i) => {
        const y = toY(v);
        ctx.strokeStyle = colors.gridColor;
        ctx.beginPath();
        ctx.moveTo(pad.left, y);
        ctx.lineTo(pad.left + plotW, y);
        ctx.stroke();
        if (instrument) {
          ctx.strokeStyle = colors.gridStrong;
          ctx.beginPath();
          ctx.moveTo(pad.left - 4, y);
          ctx.lineTo(pad.left, y);
          ctx.stroke();
        }
        ctx.fillStyle = colors.textMuted;
        ctx.font = canvasFont(10);
        ctx.textAlign = 'right';
        ctx.textBaseline = 'middle';
        ctx.fillText(yLabels[i], pad.left - (instrument ? 8 : 6), y);
      });

      // Time labels: the first is left-aligned and the last right-aligned so
      // neither runs past the plot edge. A span under ten minutes labels to the
      // second, otherwise every tick would read the same minute.
      const seconds = tRange < 10 * 60 * 1000;
      ctx.textBaseline = 'alphabetic';
      for (let i = 0; i <= xTicks; i++) {
        const t = minT + (i / xTicks) * tRange;
        const x = toX(t);
        ctx.strokeStyle = colors.gridColor;
        ctx.beginPath();
        ctx.moveTo(x, pad.top);
        ctx.lineTo(x, plotBottom);
        ctx.stroke();
        if (instrument) {
          ctx.strokeStyle = colors.gridStrong;
          ctx.beginPath();
          ctx.moveTo(x, plotBottom);
          ctx.lineTo(x, plotBottom + 4);
          ctx.stroke();
        }
        ctx.fillStyle = colors.textMuted;
        ctx.font = canvasFont(10);
        ctx.textAlign = i === 0 ? 'left' : i === xTicks ? 'right' : 'center';
        ctx.fillText(formatTime(t, { seconds }), x, plotBottom + 16);
      }
    }

    // Plot frame
    ctx.strokeStyle = instrument ? colors.gridStrong : colors.gridColor;
    ctx.lineWidth = 1;
    ctx.strokeRect(pad.left, pad.top, plotW, plotH);

    // Animate progress
    const progress = animated ? Math.min(progressRef.current, 1) : 1;

    // Draw series
    series.forEach((s, si) => {
      if (s.samples.length < 2) return;
      const color = s.color || palette[si % palette.length];

      ctx.strokeStyle = color;
      ctx.lineWidth = 1.5;
      ctx.lineJoin = 'round';

      const drawCount = Math.floor(s.samples.length * progress);

      ctx.beginPath();
      for (let i = 0; i < drawCount; i++) {
        const p = s.samples[i];
        const x = toX(p.timestamp);
        const y = toY(p.value);
        if (i === 0) ctx.moveTo(x, y);
        else ctx.lineTo(x, y);
      }
      ctx.stroke();

      // Calm area fill. In instrument mode only the primary accent trace is
      // filled, so a multi-series plot stays legible rather than muddy.
      if (drawCount > 1 && (!instrument || si === 0)) {
        ctx.globalAlpha = instrument ? 0.07 : 0.08;
        ctx.fillStyle = color;
        ctx.beginPath();
        ctx.moveTo(toX(s.samples[0].timestamp), toY(s.samples[0].value));
        for (let i = 1; i < drawCount; i++) {
          ctx.lineTo(toX(s.samples[i].timestamp), toY(s.samples[i].value));
        }
        ctx.lineTo(toX(s.samples[drawCount - 1].timestamp), plotBottom);
        ctx.lineTo(toX(s.samples[0].timestamp), plotBottom);
        ctx.closePath();
        ctx.fill();
        ctx.globalAlpha = 1;
      }
    });

    // Crosshair (instrument only) — drawn over the traces.
    const cursor = cursorRef.current;
    if (instrument && cursor) {
      const cx = Math.max(pad.left, Math.min(toX(cursor.t), pad.left + plotW));
      ctx.save();
      ctx.strokeStyle = colors.textMuted;
      ctx.globalAlpha = 0.55;
      ctx.lineWidth = 1;
      ctx.setLineDash([3, 3]);
      ctx.beginPath();
      ctx.moveTo(cx, pad.top);
      ctx.lineTo(cx, plotBottom);
      ctx.stroke();
      ctx.setLineDash([]);
      ctx.globalAlpha = 1;

      // A marker dot per series at its nearest sample, ringed in the surface
      // colour so it reads on top of the trace.
      series.forEach((s, si) => {
        const ns = nearestSample(s.samples, cursor.t);
        if (!ns) return;
        const color = s.color || palette[si % palette.length];
        const dx = toX(ns.timestamp);
        const dy = toY(ns.value);
        ctx.beginPath();
        ctx.arc(dx, dy, 3.5, 0, Math.PI * 2);
        ctx.fillStyle = color;
        ctx.fill();
        ctx.lineWidth = 1.5;
        ctx.strokeStyle = colors.surface;
        ctx.stroke();
      });
      ctx.restore();
    }

    // Legend — a short line swatch per series, wrapped into rows beneath the
    // time labels so the two never share a line.
    if (legendItems > 0) {
      ctx.font = canvasFont(10);
      ctx.textAlign = 'left';
      ctx.textBaseline = 'middle';
      const firstRowY = plotBottom + 22 + 6 + LEGEND_ROW_H / 2;
      let lx = pad.left;
      let row = 0;

      for (let i = 0; i < legendItems; i++) {
        const color = series[i].color || palette[i % palette.length];
        const label = series[i].label.length > 28
          ? series[i].label.slice(0, 26) + '..'
          : series[i].label;
        const itemWidth = LEGEND_SWATCH_W + 6 + ctx.measureText(label).width + 16;

        // Wrap to next row if this item would overflow
        if (lx + itemWidth > pad.left + plotW && lx > pad.left) {
          row++;
          lx = pad.left;
        }

        const ly = firstRowY + row * LEGEND_ROW_H;
        ctx.strokeStyle = color;
        ctx.lineWidth = 2;
        ctx.beginPath();
        ctx.moveTo(lx, ly);
        ctx.lineTo(lx + LEGEND_SWATCH_W, ly);
        ctx.stroke();
        ctx.fillStyle = colors.textMuted;
        ctx.fillText(label, lx + LEGEND_SWATCH_W + 6, ly);
        lx += itemWidth;
      }
      ctx.textBaseline = 'alphabetic';
    }
  }, [series, showGrid, showLegend, animated, instrument, baseline]);

  renderRef.current = render;

  // Recompute the snapped cursor + readout for the current pointer position,
  // coalesced to one update per frame. Reads everything from refs so the
  // handler identity is stable across data ticks.
  const updateHover = useCallback(() => {
    hoverRafRef.current = 0;
    const geom = geomRef.current;
    if (!geom) return;
    const data = seriesRef.current;
    const primary = data.find((s) => s.samples.length > 0);
    if (!primary) {
      cursorRef.current = null;
      setReadout(null);
      return;
    }
    const { pad, plotW, minT, tRange } = geom;
    const xc = Math.max(pad.left, Math.min(pointerXRef.current, pad.left + plotW));
    const tCursor = minT + ((xc - pad.left) / plotW) * tRange;
    const snap = primary.samples[nearestSampleIndex(primary.samples, tCursor)];
    cursorRef.current = { t: snap.timestamp };

    const palette = paletteRef.current;
    const points: ReadoutPoint[] = [];
    data.forEach((s, i) => {
      const ns = nearestSample(s.samples, snap.timestamp);
      if (ns) points.push({ label: s.label, color: s.color || palette[i % palette.length], value: ns.value });
    });
    renderRef.current();
    setReadout({ t: snap.timestamp, points });
  }, []);

  const handlePointerMove = useCallback(
    (e: React.PointerEvent<HTMLCanvasElement>) => {
      const canvas = canvasRef.current;
      if (!canvas || !geomRef.current) return;
      pointerXRef.current = e.clientX - canvas.getBoundingClientRect().left;
      if (hoverRafRef.current === 0) hoverRafRef.current = requestAnimationFrame(updateHover);
    },
    [updateHover],
  );

  const clearHover = useCallback(() => {
    if (hoverRafRef.current !== 0) {
      cancelAnimationFrame(hoverRafRef.current);
      hoverRafRef.current = 0;
    }
    if (cursorRef.current) {
      cursorRef.current = null;
      renderRef.current();
    }
    setReadout(null);
  }, []);

  useEffect(() => {
    const hasData = series.some((s) => s.samples.length > 0);

    // Sweep in once, the first time real data arrives, unless the viewer asked
    // for reduced motion. A one-shot latch animates exactly once (not on every
    // empty→refill), and an idle chart schedules no frames.
    if (animated && hasData && !hasAnimatedRef.current && !prefersReducedMotion()) {
      hasAnimatedRef.current = true;
      progressRef.current = 0;
      const start = performance.now();
      const duration = 600;

      const animate = (now: number) => {
        progressRef.current = Math.min((now - start) / duration, 1);
        render();
        if (progressRef.current < 1) {
          sweepRafRef.current = requestAnimationFrame(animate);
        }
      };
      sweepRafRef.current = requestAnimationFrame(animate);
    } else {
      progressRef.current = 1;
      render();
    }

    return () => cancelAnimationFrame(sweepRafRef.current);
  }, [series, render, animated]);

  // Data changed — a held crosshair would point at a stale time, so drop it.
  useEffect(() => {
    cursorRef.current = null;
    setReadout(null);
  }, [series]);

  // Resize handling
  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;
    const ro = new ResizeObserver(() => {
      progressRef.current = 1;
      render();
    });
    ro.observe(container);
    return () => ro.disconnect();
  }, [render]);

  return (
    <div ref={containerRef} className="relative w-full h-full min-h-0">
      <canvas
        ref={canvasRef}
        className={`block w-full h-full ${instrument ? 'cursor-crosshair' : ''}`}
        onPointerMove={instrument ? handlePointerMove : undefined}
        onPointerLeave={instrument ? clearHover : undefined}
        onPointerCancel={instrument ? clearHover : undefined}
      />
      {instrument && readout && readout.points.length > 0 && (
        <div className="pointer-events-none absolute top-2 right-2 rounded-md border bg-surface px-2.5 py-1.5 max-w-[55%]">
          <div className="text-2xs font-mono tabular-nums text-muted mb-1">{formatTime(readout.t)}</div>
          <div className="space-y-0.5">
            {readout.points.slice(0, MAX_READOUT_ROWS).map((p, i) => (
              <div key={`${p.label}-${i}`} className="flex items-center gap-2 text-2xs font-mono tabular-nums">
                <span className="w-3 h-0.5 shrink-0" style={{ backgroundColor: p.color }} />
                <span className="text-muted truncate">{p.label}</span>
                <span className="ml-auto pl-2 text-text">{formatNumber(p.value)}</span>
              </div>
            ))}
            {readout.points.length > MAX_READOUT_ROWS && (
              <div className="text-2xs font-mono text-muted">+{readout.points.length - MAX_READOUT_ROWS} more</div>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
