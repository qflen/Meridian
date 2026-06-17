import { useRef, useEffect, useCallback } from 'react';
import { Sample } from '../types';
import { getCanvasColors } from '../utils/canvasColors';
import { canvasFont } from '../utils/canvasFont';

// Palette of visually distinct colors for multi-series
const COLORS = [
  '#5c7cfa', '#22c55e', '#f59e0b', '#ef4444', '#a855f7',
  '#06b6d4', '#ec4899', '#84cc16', '#f97316', '#6366f1',
];

interface SeriesData {
  label: string;
  samples: Sample[];
  color?: string;
}

interface Props {
  series: SeriesData[];
  width?: number;
  height?: number;
  showGrid?: boolean;
  showLegend?: boolean;
  animated?: boolean;
  yLabel?: string;
  title?: string;
}

function formatValue(v: number): string {
  if (Math.abs(v) >= 1e9) return (v / 1e9).toFixed(1) + 'G';
  if (Math.abs(v) >= 1e6) return (v / 1e6).toFixed(1) + 'M';
  if (Math.abs(v) >= 1e3) return (v / 1e3).toFixed(1) + 'K';
  if (Math.abs(v) < 0.01 && v !== 0) return v.toExponential(1);
  return v.toFixed(v % 1 === 0 ? 0 : 2);
}

function formatTime(ts: number): string {
  const d = new Date(ts);
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

export function TimeSeriesChart({
  series,
  width: propWidth,
  height: propHeight,
  showGrid = true,
  showLegend = true,
  animated = true,
  yLabel,
  title,
}: Props) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const rafRef = useRef(0);
  const progressRef = useRef(0);
  const hasAnimatedRef = useRef(false);

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

    const legendRows = showLegend && series.length > 0 ? Math.ceil(series.length / 3) : 0;
    const pad = { top: title ? 32 : 16, right: 16, bottom: 28 + legendRows * 16, left: 56 };
    const plotW = w - pad.left - pad.right;
    const plotH = h - pad.top - pad.bottom;

    // Resolve CSS variables for canvas (canvas cannot use var() directly)
    const colors = getCanvasColors(canvas);

    // Clear
    ctx.clearRect(0, 0, w, h);

    // Title
    if (title) {
      ctx.fillStyle = colors.textMuted;
      ctx.font = canvasFont(11, { family: 'sans' });
      ctx.textAlign = 'left';
      ctx.fillText(title, pad.left, 14);
    }

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
      // No data - draw empty state
      ctx.fillStyle = colors.textMuted;
      ctx.font = canvasFont(13, { family: 'sans' });
      ctx.textAlign = 'center';
      ctx.fillText('No data', w / 2, h / 2);
      return;
    }

    // Padding for Y range
    const vRange = maxV - minV || 1;
    minV -= vRange * 0.05;
    maxV += vRange * 0.05;
    const tRange = maxT - minT || 1;

    const toX = (t: number) => pad.left + ((t - minT) / tRange) * plotW;
    const toY = (v: number) => pad.top + plotH - ((v - minV) / (maxV - minV)) * plotH;

    // Grid
    if (showGrid) {
      ctx.strokeStyle = colors.gridColor;
      ctx.lineWidth = 0.5;

      // Horizontal grid lines
      const yTicks = 5;
      for (let i = 0; i <= yTicks; i++) {
        const v = minV + (i / yTicks) * (maxV - minV);
        const y = toY(v);
        ctx.beginPath();
        ctx.moveTo(pad.left, y);
        ctx.lineTo(pad.left + plotW, y);
        ctx.stroke();

        ctx.fillStyle = colors.textMuted;
        ctx.font = canvasFont(10);
        ctx.textAlign = 'right';
        ctx.fillText(formatValue(v), pad.left - 6, y + 3);
      }

      // Vertical grid lines
      const xTicks = Math.min(6, Math.max(2, Math.floor(plotW / 100)));
      for (let i = 0; i <= xTicks; i++) {
        const t = minT + (i / xTicks) * tRange;
        const x = toX(t);
        ctx.beginPath();
        ctx.moveTo(x, pad.top);
        ctx.lineTo(x, pad.top + plotH);
        ctx.stroke();

        ctx.fillStyle = colors.textMuted;
        ctx.font = canvasFont(10);
        ctx.textAlign = 'center';
        ctx.fillText(formatTime(t), x, pad.top + plotH + 16);
      }
    }

    // Y-axis label
    if (yLabel) {
      ctx.save();
      ctx.fillStyle = colors.textMuted;
      ctx.font = canvasFont(10, { family: 'sans' });
      ctx.translate(12, pad.top + plotH / 2);
      ctx.rotate(-Math.PI / 2);
      ctx.textAlign = 'center';
      ctx.fillText(yLabel, 0, 0);
      ctx.restore();
    }

    // Plot border
    ctx.strokeStyle = colors.gridColor;
    ctx.lineWidth = 1;
    ctx.strokeRect(pad.left, pad.top, plotW, plotH);

    // Animate progress
    const progress = animated ? Math.min(progressRef.current, 1) : 1;

    // Draw series
    series.forEach((s, si) => {
      if (s.samples.length < 2) return;
      const color = s.color || COLORS[si % COLORS.length];

      // Glow effect
      ctx.save();
      ctx.shadowColor = color;
      ctx.shadowBlur = 6;
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
      ctx.restore();

      // Area fill
      if (drawCount > 1) {
        ctx.globalAlpha = 0.08;
        ctx.fillStyle = color;
        ctx.beginPath();
        ctx.moveTo(toX(s.samples[0].timestamp), toY(s.samples[0].value));
        for (let i = 1; i < drawCount; i++) {
          ctx.lineTo(toX(s.samples[i].timestamp), toY(s.samples[i].value));
        }
        ctx.lineTo(toX(s.samples[drawCount - 1].timestamp), pad.top + plotH);
        ctx.lineTo(toX(s.samples[0].timestamp), pad.top + plotH);
        ctx.closePath();
        ctx.fill();
        ctx.globalAlpha = 1;
      }
    });

    // Legend — render in rows so items never overlap
    if (showLegend && series.length > 0) {
      ctx.font = canvasFont(10, { family: 'sans' });
      const legendBaseY = pad.top + plotH + 24;
      let lx = pad.left;
      let row = 0;

      for (let i = 0; i < Math.min(series.length, 12); i++) {
        const color = series[i].color || COLORS[i % COLORS.length];
        const label = series[i].label.length > 28
          ? series[i].label.slice(0, 26) + '..'
          : series[i].label;
        const itemWidth = 14 + ctx.measureText(label).width + 16;

        // Wrap to next row if this item would overflow
        if (lx + itemWidth > pad.left + plotW && lx > pad.left) {
          row++;
          lx = pad.left;
        }

        const ly = legendBaseY + row * 16;
        ctx.fillStyle = color;
        ctx.fillRect(lx, ly - 3, 8, 8);
        ctx.fillStyle = colors.textMuted;
        ctx.fillText(label, lx + 14, ly + 4);
        lx += itemWidth;
      }
    }
  }, [series, showGrid, showLegend, animated, yLabel, title]);

  useEffect(() => {
    const hasData = series.some((s) => s.samples.length > 0);

    // Sweep in once, the first time real data arrives. Inferring "first data"
    // from the previous series length re-ran the 600ms sweep every time a chart
    // emptied and refilled; a one-shot latch animates exactly once.
    if (animated && hasData && !hasAnimatedRef.current) {
      hasAnimatedRef.current = true;
      progressRef.current = 0;
      const start = performance.now();
      const duration = 600;

      const animate = (now: number) => {
        progressRef.current = Math.min((now - start) / duration, 1);
        render();
        if (progressRef.current < 1) {
          rafRef.current = requestAnimationFrame(animate);
        }
      };
      rafRef.current = requestAnimationFrame(animate);
    } else {
      // Subsequent updates: render immediately without animation
      progressRef.current = 1;
      render();
    }

    return () => cancelAnimationFrame(rafRef.current);
  }, [series, render, animated]);

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
    <div ref={containerRef} className="w-full" style={{ height: propHeight || 240 }}>
      <canvas
        ref={canvasRef}
        className="w-full h-full"
        style={{ width: propWidth || '100%', height: propHeight || 240 }}
      />
    </div>
  );
}
