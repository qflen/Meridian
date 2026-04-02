import { useRef, useEffect, useCallback } from 'react';
import { ClusterNode } from '../types';
import { useDashboard } from '../state/DashboardContext';

const STATE_COLORS: Record<string, string> = {
  active: '#22c55e',
  joining: '#f59e0b',
  leaving: '#f97316',
  dead: '#ef4444',
};

export function ClusterTopology() {
  const { state } = useDashboard();
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const animRef = useRef(0);
  const pulseRef = useRef(0);

  // Demo nodes when not connected
  const nodes: ClusterNode[] =
    state.clusterNodes.length > 0
      ? state.clusterNodes
      : [
          { id: 'node-1', address: ':8080', state: 'active', series: 1200, samples: 84000 },
          { id: 'node-2', address: ':8081', state: 'active', series: 980, samples: 71000 },
          { id: 'node-3', address: ':8082', state: 'active', series: 1100, samples: 79000 },
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

    const cx = w / 2;
    const cy = h / 2;
    const radius = Math.min(w, h) * 0.32;
    const pulse = Math.sin(pulseRef.current) * 0.5 + 0.5;

    // Draw ring
    ctx.strokeStyle = 'var(--grid-color)';
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.arc(cx, cy, radius, 0, Math.PI * 2);
    ctx.stroke();

    // Draw connections between active nodes
    const activeNodes = nodes.filter((n) => n.state === 'active');
    ctx.strokeStyle = 'rgba(92, 124, 250, 0.1)';
    ctx.lineWidth = 0.5;
    for (let i = 0; i < activeNodes.length; i++) {
      for (let j = i + 1; j < activeNodes.length; j++) {
        const ai = nodes.indexOf(activeNodes[i]);
        const aj = nodes.indexOf(activeNodes[j]);
        const a1 = (ai / nodes.length) * Math.PI * 2 - Math.PI / 2;
        const a2 = (aj / nodes.length) * Math.PI * 2 - Math.PI / 2;
        ctx.beginPath();
        ctx.moveTo(cx + Math.cos(a1) * radius, cy + Math.sin(a1) * radius);
        ctx.lineTo(cx + Math.cos(a2) * radius, cy + Math.sin(a2) * radius);
        ctx.stroke();
      }
    }

    // Draw nodes
    nodes.forEach((node, i) => {
      const angle = (i / nodes.length) * Math.PI * 2 - Math.PI / 2;
      const nx = cx + Math.cos(angle) * radius;
      const ny = cy + Math.sin(angle) * radius;
      const color = STATE_COLORS[node.state] || '#666';

      // Glow
      if (node.state === 'active') {
        ctx.save();
        ctx.shadowColor = color;
        ctx.shadowBlur = 12 + pulse * 6;
        ctx.fillStyle = color;
        ctx.beginPath();
        ctx.arc(nx, ny, 6, 0, Math.PI * 2);
        ctx.fill();
        ctx.restore();
      }

      // Node circle
      ctx.fillStyle = color;
      ctx.beginPath();
      ctx.arc(nx, ny, node.state === 'active' ? 8 : 6, 0, Math.PI * 2);
      ctx.fill();

      // Border
      ctx.strokeStyle = 'rgba(0,0,0,0.3)';
      ctx.lineWidth = 1.5;
      ctx.stroke();

      // Label
      ctx.fillStyle = 'rgb(var(--color-text-muted))';
      ctx.font = '10px Inter, sans-serif';
      ctx.textAlign = 'center';
      const ly = ny > cy ? ny + 20 : ny - 14;
      ctx.fillText(node.id, nx, ly);
      ctx.font = '9px Inter, sans-serif';
      ctx.fillText(`${node.series} series`, nx, ly + 12);
    });

    // Center label
    ctx.fillStyle = 'rgb(var(--color-text))';
    ctx.font = 'bold 14px Inter, sans-serif';
    ctx.textAlign = 'center';
    ctx.fillText(`${nodes.length}`, cx, cy - 4);
    ctx.fillStyle = 'rgb(var(--color-text-muted))';
    ctx.font = '10px Inter, sans-serif';
    ctx.fillText('nodes', cx, cy + 10);

    pulseRef.current += 0.03;
    animRef.current = requestAnimationFrame(render);
  }, [nodes]);

  useEffect(() => {
    animRef.current = requestAnimationFrame(render);
    return () => cancelAnimationFrame(animRef.current);
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
      <h3 className="text-sm font-semibold text-gray-300 mb-2">Cluster Topology</h3>
      <div ref={containerRef} className="w-full" style={{ height: 240 }}>
        <canvas ref={canvasRef} className="w-full h-full" style={{ height: 240 }} />
      </div>
      <div className="flex gap-4 mt-2 justify-center">
        {Object.entries(STATE_COLORS).map(([label, color]) => (
          <div key={label} className="flex items-center gap-1.5">
            <span
              className="inline-block w-2 h-2 rounded-full"
              style={{ backgroundColor: color }}
            />
            <span className="text-xs text-gray-500 capitalize">{label}</span>
          </div>
        ))}
      </div>
    </div>
  );
}
