import { useRef, useEffect, useCallback, useState } from 'react';
import { getCanvasColors } from '../utils/canvasColors';

interface Bucket {
  le: string;
  count: number;
}

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

  // Demo buckets when no data
  const displayBuckets =
    buckets.length > 0 && buckets.some((b) => b.count > 0)
      ? buckets
      : [
          { le: '1ms', count: 0 },
          { le: '5ms', count: 0 },
          { le: '10ms', count: 0 },
          { le: '25ms', count: 0 },
          { le: '50ms', count: 0 },
          { le: '100ms', count: 0 },
          { le: '250ms', count: 0 },
          { le: '500ms', count: 0 },
          { le: '1s', count: 0 },
        ];

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
    const pad = { top: 8, right: 12, bottom: 28, left: 40 };
    const plotW = w - pad.left - pad.right;
    const plotH = h - pad.top - pad.bottom;
    const barW = Math.max(4, plotW / displayBuckets.length - 4);
    const maxCount = Math.max(...displayBuckets.map((b) => b.count), 1);

    // Draw "awaiting data" if all zeros
    if (displayBuckets.every((b) => b.count === 0)) {
      ctx.fillStyle = colors.textMuted;
      ctx.font = '11px Inter, sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText('Run a query to see latency distribution', w / 2, h / 2);

      // Still draw x-axis labels
      displayBuckets.forEach((bucket, i) => {
        const x = pad.left + (i / displayBuckets.length) * plotW + 2;
        ctx.fillStyle = colors.textMuted;
        ctx.font = '9px Inter, sans-serif';
        ctx.textAlign = 'center';
        ctx.fillText(bucket.le, x + barW / 2, pad.top + plotH + 14);
      });
      return;
    }

    displayBuckets.forEach((bucket, i) => {
      const barH = (bucket.count / maxCount) * plotH;
      const x = pad.left + (i / displayBuckets.length) * plotW + 2;
      const y = pad.top + plotH - barH;

      // Bar gradient
      const gradient = ctx.createLinearGradient(x, y, x, y + barH);
      gradient.addColorStop(0, '#5c7cfa');
      gradient.addColorStop(1, '#4263eb');
      ctx.fillStyle = gradient;

      // Rounded top
      const cornerR = Math.min(3, barW / 2);
      ctx.beginPath();
      ctx.moveTo(x + cornerR, y);
      ctx.lineTo(x + barW - cornerR, y);
      ctx.quadraticCurveTo(x + barW, y, x + barW, y + cornerR);
      ctx.lineTo(x + barW, y + barH);
      ctx.lineTo(x, y + barH);
      ctx.lineTo(x, y + cornerR);
      ctx.quadraticCurveTo(x, y, x + cornerR, y);
      ctx.fill();

      // Glow
      ctx.save();
      ctx.shadowColor = '#5c7cfa';
      ctx.shadowBlur = 4;
      ctx.fillRect(x, y, barW, 1);
      ctx.restore();

      // Label
      ctx.fillStyle = colors.textMuted;
      ctx.font = '9px Inter, sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText(bucket.le, x + barW / 2, pad.top + plotH + 14);
    });

    // Y axis ticks
    ctx.fillStyle = colors.textMuted;
    ctx.font = '9px Inter, sans-serif';
    ctx.textAlign = 'right';
    for (let i = 0; i <= 4; i++) {
      const v = Math.round((maxCount / 4) * i);
      const y = pad.top + plotH - (i / 4) * plotH;
      ctx.fillText(String(v), pad.left - 6, y + 3);
    }
  }, [displayBuckets]);

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

  return (
    <div className="card h-[294px]">
      <h3 className="text-sm font-semibold mb-2" style={{ color: 'rgb(var(--color-text))' }}>Query Latency Distribution</h3>
      <div ref={containerRef} style={{ height: 180 }}>
        <canvas ref={canvasRef} className="w-full h-full" style={{ height: 180 }} />
      </div>
    </div>
  );
}
