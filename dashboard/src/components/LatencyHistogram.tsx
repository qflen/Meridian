import { useRef, useEffect, useCallback, useState } from 'react';

interface Bucket {
  le: string;
  count: number;
}

export function LatencyHistogram() {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const [buckets, setBuckets] = useState<Bucket[]>([]);

  // Fetch histogram data periodically
  useEffect(() => {
    const fetchData = () => {
      fetch('/api/query?q=query_duration_seconds_bucket&format=json')
        .then((r) => r.json())
        .then((data) => {
          if (data?.data) {
            const bkts: Bucket[] = data.data
              .filter((s: { labels?: Record<string, string> }) => s.labels?.le)
              .map((s: { labels: Record<string, string>; samples: { value: number }[] }) => ({
                le: s.labels.le,
                count: s.samples?.[s.samples.length - 1]?.value ?? 0,
              }));
            if (bkts.length > 0) setBuckets(bkts);
          }
        })
        .catch(() => {});
    };

    fetchData();
    const interval = setInterval(fetchData, 10000);
    return () => clearInterval(interval);
  }, []);

  // Demo buckets when no data
  const displayBuckets =
    buckets.length > 0
      ? buckets
      : [
          { le: '1ms', count: 420 },
          { le: '5ms', count: 380 },
          { le: '10ms', count: 290 },
          { le: '25ms', count: 160 },
          { le: '50ms', count: 85 },
          { le: '100ms', count: 32 },
          { le: '250ms', count: 12 },
          { le: '500ms', count: 4 },
          { le: '1s', count: 1 },
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

    const pad = { top: 8, right: 12, bottom: 28, left: 40 };
    const plotW = w - pad.left - pad.right;
    const plotH = h - pad.top - pad.bottom;
    const barW = Math.max(4, plotW / displayBuckets.length - 4);
    const maxCount = Math.max(...displayBuckets.map((b) => b.count), 1);

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
      ctx.fillStyle = 'rgb(var(--color-text-muted))';
      ctx.font = '9px Inter, sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText(bucket.le, x + barW / 2, pad.top + plotH + 14);
    });

    // Y axis ticks
    ctx.fillStyle = 'rgb(var(--color-text-muted))';
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
    <div className="card">
      <h3 className="text-sm font-semibold text-gray-300 mb-2">Query Latency Distribution</h3>
      <div ref={containerRef} style={{ height: 180 }}>
        <canvas ref={canvasRef} className="w-full h-full" style={{ height: 180 }} />
      </div>
    </div>
  );
}
