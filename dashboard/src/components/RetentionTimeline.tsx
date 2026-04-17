import { useRef, useEffect, useCallback, useState } from 'react';
import { useDashboard } from '../state/DashboardContext';
import { getCanvasColors } from '../utils/canvasColors';
import { BlockInfo } from '../types';

export function RetentionTimeline() {
  const { state } = useDashboard();
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const [blocks, setBlocks] = useState<BlockInfo[]>([]);

  // Fetch real block data from the API
  useEffect(() => {
    let cancelled = false;

    const fetchBlocks = async () => {
      try {
        const res = await fetch('/api/v1/blocks');
        const data = await res.json();
        if (!cancelled && data.blocks) {
          setBlocks(data.blocks);
        }
      } catch {
        // ignore
      }
    };

    fetchBlocks();
    const interval = setInterval(fetchBlocks, 10000);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, []);

  // Build display blocks: real blocks + head block
  const now = Date.now();
  const displayBlocks = (() => {
    const result: { id: string; node: string; minTime: number; maxTime: number; samples: number }[] = [];

    if (blocks.length > 0) {
      for (const b of blocks) {
        result.push({
          id: b.ulid.substring(0, 8),
          node: b.node_id,
          minTime: b.min_time,
          maxTime: b.max_time,
          samples: b.num_samples,
        });
      }
    }

    // Always show a head block for current time
    const headStart = result.length > 0
      ? Math.max(...result.map((b) => b.maxTime))
      : now - 3600000;
    result.push({
      id: 'head',
      node: 'all',
      minTime: headStart,
      maxTime: now,
      samples: state.stats?.activeSeries ?? 0,
    });

    return result;
  })();

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
    const pad = { top: 16, right: 16, bottom: 24, left: 56 };
    const plotW = w - pad.left - pad.right;
    const plotH = h - pad.top - pad.bottom;

    if (displayBlocks.length === 0) return;

    const minT = Math.min(...displayBlocks.map((b) => b.minTime));
    const maxT = Math.max(...displayBlocks.map((b) => b.maxTime));
    const tRange = maxT - minT || 1;

    // Group blocks by storage node for multi-lane display
    const nodeIds = [...new Set(displayBlocks.map((b) => b.node))];
    const laneCount = Math.max(nodeIds.length, 1);
    const laneH = plotH / laneCount;

    const nodeColors = ['#5c7cfa', '#22c55e', '#f59e0b', '#a855f7', '#f97316'];

    nodeIds.forEach((nodeId, lane) => {
      const y = pad.top + lane * laneH;
      const color = nodeColors[lane % nodeColors.length];

      // Lane label
      ctx.fillStyle = colors.textMuted;
      ctx.font = '9px Inter, sans-serif';
      ctx.textAlign = 'right';
      const label = nodeId === 'all' ? 'head' : nodeId.replace('storage-', 'S');
      ctx.fillText(label, pad.left - 8, y + laneH / 2 + 3);

      // Lane background
      ctx.fillStyle = lane % 2 === 0 ? 'rgba(255,255,255,0.02)' : 'transparent';
      ctx.fillRect(pad.left, y, plotW, laneH);

      // Blocks in this lane
      displayBlocks
        .filter((b) => b.node === nodeId)
        .forEach((b) => {
          const x1 = pad.left + ((b.minTime - minT) / tRange) * plotW;
          const x2 = pad.left + ((b.maxTime - minT) / tRange) * plotW;
          const bw = Math.max(x2 - x1, 4);

          ctx.fillStyle = color;
          ctx.globalAlpha = b.id === 'head' ? 0.5 : 0.3;
          ctx.fillRect(x1 + 1, y + 4, bw - 2, laneH - 8);
          ctx.globalAlpha = 1;

          // Top accent
          ctx.fillStyle = color;
          ctx.fillRect(x1 + 1, y + 4, bw - 2, 2);

          // Block id
          if (bw > 40) {
            ctx.fillStyle = colors.textMuted;
            ctx.font = '8px Inter, sans-serif';
            ctx.textAlign = 'center';
            ctx.fillText(b.id, x1 + bw / 2, y + laneH / 2 + 3);
          }
        });

      // Lane separator
      ctx.strokeStyle = colors.gridColor;
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
      ctx.fillStyle = colors.textMuted;
      ctx.font = '9px Inter, sans-serif';
      ctx.textAlign = 'center';
      ctx.fillText(d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }), x, pad.top + plotH + 16);
    }
  }, [displayBlocks]);

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
        <h3 className="text-sm font-semibold" style={{ color: 'rgb(var(--color-text))' }}>Retention Timeline</h3>
        <span className="text-xs" style={{ color: 'rgb(var(--color-text-muted))' }}>
          {blocks.length > 0 ? `${blocks.length} blocks` : state.stats ? `${state.stats.blockCount} blocks` : '--'}
        </span>
      </div>
      <div ref={containerRef} style={{ height: 160 }}>
        <canvas ref={canvasRef} className="w-full h-full" style={{ height: 160 }} />
      </div>
    </div>
  );
}
