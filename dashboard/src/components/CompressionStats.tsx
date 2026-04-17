import { useRef, useEffect, useCallback } from 'react';
import { useDashboard } from '../state/DashboardContext';
import { getCanvasColors } from '../utils/canvasColors';

function formatBytes(b: number): string {
  if (b >= 1e9) return (b / 1e9).toFixed(2) + ' GB';
  if (b >= 1e6) return (b / 1e6).toFixed(2) + ' MB';
  if (b >= 1e3) return (b / 1e3).toFixed(2) + ' KB';
  return b + ' B';
}

export function CompressionStats() {
  const { state } = useDashboard();
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);

  const stats = state.stats;
  const ratio =
    stats && stats.rawBytes > 0 ? stats.rawBytes / stats.compressedBytes : 0;

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
    const cx = w / 2;
    const cy = h / 2 + 8;
    const r = Math.min(w, h) * 0.35;

    // Background arc
    ctx.lineWidth = 12;
    ctx.lineCap = 'round';
    ctx.strokeStyle = colors.gridColor;
    ctx.beginPath();
    ctx.arc(cx, cy, r, Math.PI * 0.8, Math.PI * 2.2);
    ctx.stroke();

    // Ratio arc (~30x covers well-populated head + flushed blocks)
    const maxRatio = 30;
    const progress = Math.min(ratio / maxRatio, 1);
    const startAngle = Math.PI * 0.8;
    const endAngle = startAngle + progress * Math.PI * 1.4;

    const gradient = ctx.createLinearGradient(cx - r, cy, cx + r, cy);
    gradient.addColorStop(0, '#5c7cfa');
    gradient.addColorStop(0.5, '#22c55e');
    gradient.addColorStop(1, '#f59e0b');

    ctx.strokeStyle = gradient;
    ctx.lineWidth = 12;
    ctx.lineCap = 'round';
    ctx.beginPath();
    ctx.arc(cx, cy, r, startAngle, endAngle);
    ctx.stroke();

    // Center text
    ctx.fillStyle = colors.text;
    ctx.font = 'bold 22px Inter, sans-serif';
    ctx.textAlign = 'center';
    ctx.fillText(ratio > 0 ? `${ratio.toFixed(1)}x` : '--', cx, cy + 2);
    ctx.fillStyle = colors.textMuted;
    ctx.font = '10px Inter, sans-serif';
    ctx.fillText('compression ratio', cx, cy + 18);
  }, [ratio]);

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
      <h3 className="text-sm font-semibold mb-2" style={{ color: 'rgb(var(--color-text))' }}>Gorilla Compression</h3>
      <div ref={containerRef} style={{ height: 160 }}>
        <canvas ref={canvasRef} className="w-full h-full" style={{ height: 160 }} />
      </div>
      <div className="grid grid-cols-2 gap-3 mt-2">
        <div className="text-center">
          <div className="text-sm font-bold" style={{ color: 'rgb(var(--color-text))' }}>
            {stats ? formatBytes(stats.rawBytes) : '--'}
          </div>
          <div className="stat-label">raw size</div>
        </div>
        <div className="text-center">
          <div className="text-sm font-bold text-meridian-400">
            {stats ? formatBytes(stats.compressedBytes) : '--'}
          </div>
          <div className="stat-label">compressed</div>
        </div>
      </div>
    </div>
  );
}
