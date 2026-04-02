import { useRef, useEffect, useCallback } from 'react';
import { useDashboard } from '../state/DashboardContext';

interface Block {
  id: string;
  minTime: number;
  maxTime: number;
  samples: number;
  resolution: '5s' | '1m' | '1h';
}

export function RetentionTimeline() {
  const { state } = useDashboard();
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);

  // Demo blocks when no real data
  const now = Date.now();
  const blocks: Block[] = [
    { id: 'blk-1', minTime: now - 3600000 * 4, maxTime: now - 3600000 * 3, samples: 72000, resolution: '5s' },
    { id: 'blk-2', minTime: now - 3600000 * 3, maxTime: now - 3600000 * 2, samples: 72000, resolution: '5s' },
    { id: 'blk-3', minTime: now - 3600000 * 2, maxTime: now - 3600000, samples: 72000, resolution: '5s' },
    { id: 'head', minTime: now - 3600000, maxTime: now, samples: state.stats?.activeSeries ?? 43000, resolution: '5s' },
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

    const pad = { top: 16, right: 16, bottom: 24, left: 56 };
    const plotW = w - pad.left - pad.right;
    const plotH = h - pad.top - pad.bottom;

    if (blocks.length === 0) return;

    const minT = Math.min(...blocks.map((b) => b.minTime));
    const maxT = Math.max(...blocks.map((b) => b.maxTime));
    const tRange = maxT - minT || 1;

    const resColors: Record<string, string> = {
      '5s': '#5c7cfa',
      '1m': '#22c55e',
      '1h': '#f59e0b',
    };

    const resLabels = ['5s', '1m', '1h'];

    // Draw resolution lanes
    resLabels.forEach((res, lane) => {
      const laneH = plotH / 3;
      const y = pad.top + lane * laneH;

      // Lane label
      ctx.fillStyle = 'rgb(var(--color-text-muted))';
      ctx.font = '10px Inter, sans-serif';
      ctx.textAlign = 'right';
      ctx.fillText(res, pad.left - 8, y + laneH / 2 + 3);

      // Lane background
      ctx.fillStyle = lane % 2 === 0 ? 'rgba(255,255,255,0.02)' : 'transparent';
      ctx.fillRect(pad.left, y, plotW, laneH);

      // Blocks in this lane
      blocks
        .filter((b) => b.resolution === res)
        .forEach((b) => {
          const x1 = pad.left + ((b.minTime - minT) / tRange) * plotW;
          const x2 = pad.left + ((b.maxTime - minT) / tRange) * plotW;
          const bw = Math.max(x2 - x1, 4);

          const color = resColors[res] || '#5c7cfa';
          ctx.fillStyle = color;
          ctx.globalAlpha = 0.3;
          ctx.fillRect(x1 + 1, y + 4, bw - 2, laneH - 8);
          ctx.globalAlpha = 1;

          // Top accent
          ctx.fillStyle = color;
          ctx.fillRect(x1 + 1, y + 4, bw - 2, 2);

          // Block id
          if (bw > 40) {
            ctx.fillStyle = 'rgb(var(--color-text-muted))';
            ctx.font = '8px Inter, sans-serif';
            ctx.textAlign = 'center';
            ctx.fillText(b.id, x1 + bw / 2, y + laneH / 2 + 3);
          }
        });

      // Lane separator
      ctx.strokeStyle = 'var(--grid-color)';
      ctx.lineWidth = 0.5;
      ctx.beginPath();
      ctx.moveTo(pad.left, y + laneH);
      ctx.lineTo(pad.left + plotW, y + laneH);
      ctx.stroke();
    });

    // Time axis
    const ticks = 5;
    for (let i = 0; i <= ticks; i++) {
      const t = minT + (i / ticks) * tRange;
      const x = pad.left + (i / ticks) * plotW;
      const d = new Date(t);
      ctx.fillStyle = 'rgb(var(--color-text-muted))';
      ctx.font = '9px Inter, sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText(d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }), x, pad.top + plotH + 16);
    }
  }, [blocks]);

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
      <div className="flex items-center justify-between mb-2">
        <h3 className="text-sm font-semibold text-gray-300">Retention Timeline</h3>
        <span className="text-xs text-gray-500">
          {state.stats ? `${state.stats.blockCount} blocks` : '--'}
        </span>
      </div>
      <div ref={containerRef} style={{ height: 160 }}>
        <canvas ref={canvasRef} className="w-full h-full" style={{ height: 160 }} />
      </div>
    </div>
  );
}
