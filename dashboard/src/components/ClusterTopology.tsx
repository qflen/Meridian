import { useRef, useEffect, useCallback } from 'react';
import { ClusterNode } from '../types';
import { useDashboard } from '../state/DashboardContext';
import { getCanvasColors } from '../utils/canvasColors';

const STATE_COLORS: Record<string, string> = {
  active: '#22c55e',
  joining: '#f59e0b',
  leaving: '#f97316',
  dead: '#ef4444',
};

const ROLE_COLORS: Record<string, string> = {
  gateway: '#5c7cfa',
  ingestor: '#f59e0b',
  storage: '#22c55e',
  querier: '#a855f7',
  compactor: '#f97316',
  unknown: '#666666',
};

const ROLE_ICONS: Record<string, string> = {
  gateway: 'GW',
  ingestor: 'IN',
  storage: 'ST',
  querier: 'QR',
  compactor: 'CP',
  unknown: '??',
};

export function ClusterTopology() {
  const { state, dispatch } = useDashboard();
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const animRef = useRef(0);
  const pulseRef = useRef(0);

  // Fetch cluster data from API
  useEffect(() => {
    let cancelled = false;

    const fetchCluster = async () => {
      try {
        const res = await fetch('/api/v1/cluster');
        const data = await res.json();
        if (!cancelled && data.nodes) {
          dispatch({
            type: 'SET_CLUSTER_NODES',
            nodes: data.nodes.map((n: { id: string; addr: string; state: string; role?: string; series?: number; samples?: number }) => ({
              id: n.id,
              address: n.addr,
              state: n.state as ClusterNode['state'],
              role: (n.role ?? 'unknown') as ClusterNode['role'],
              series: n.series ?? 0,
              samples: n.samples ?? 0,
            })),
          });
        }
      } catch {
        // ignore fetch errors
      }
    };

    fetchCluster();
    const interval = setInterval(fetchCluster, 5000);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, [dispatch]);

  const nodes: ClusterNode[] = state.clusterNodes;

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
    const cy = h / 2;
    const radius = Math.min(w, h) * 0.25;
    const pulse = Math.sin(pulseRef.current) * 0.5 + 0.5;

    // Draw ring
    ctx.strokeStyle = colors.gridColor;
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.arc(cx, cy, radius, 0, Math.PI * 2);
    ctx.stroke();

    // Draw connections between services (storage ↔ querier, storage ↔ ingestor, etc.)
    const storageNodes = nodes.filter((n) => n.role === 'storage' && n.state === 'active');
    const otherActive = nodes.filter(
      (n) => n.state === 'active' && (n.role === 'ingestor' || n.role === 'querier' || n.role === 'gateway'),
    );

    ctx.lineWidth = 0.5;
    for (const other of otherActive) {
      const oi = nodes.indexOf(other);
      const a1 = (oi / nodes.length) * Math.PI * 2 - Math.PI / 2;
      for (const sn of storageNodes) {
        const si = nodes.indexOf(sn);
        const a2 = (si / nodes.length) * Math.PI * 2 - Math.PI / 2;
        const roleColor = ROLE_COLORS[other.role] || '#5c7cfa';
        ctx.strokeStyle = roleColor + '22'; // very transparent
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
      const stateColor = STATE_COLORS[node.state] || '#666';
      const roleColor = ROLE_COLORS[node.role] || '#666';
      const nodeR = node.state === 'active' ? 10 : 7;

      // Glow for active nodes
      if (node.state === 'active') {
        ctx.save();
        ctx.shadowColor = roleColor;
        ctx.shadowBlur = 10 + pulse * 5;
        ctx.fillStyle = roleColor;
        ctx.beginPath();
        ctx.arc(nx, ny, nodeR - 2, 0, Math.PI * 2);
        ctx.fill();
        ctx.restore();
      }

      // Node circle with role color
      ctx.fillStyle = node.state === 'active' ? roleColor : stateColor;
      ctx.beginPath();
      ctx.arc(nx, ny, nodeR, 0, Math.PI * 2);
      ctx.fill();

      // Border
      ctx.strokeStyle = 'rgba(0,0,0,0.3)';
      ctx.lineWidth = 1.5;
      ctx.stroke();

      // Role abbreviation inside the circle
      ctx.fillStyle = '#fff';
      ctx.font = `bold ${nodeR > 8 ? 8 : 7}px Inter, sans-serif`;
      ctx.textAlign = 'center';
      ctx.textBaseline = 'middle';
      ctx.fillText(ROLE_ICONS[node.role] || '??', nx, ny);

      // Label below/above
      ctx.fillStyle = colors.textMuted;
      ctx.font = '10px Inter, sans-serif';
      ctx.textAlign = 'center';
      ctx.textBaseline = 'alphabetic';
      const labelGap = nodeR + 14;
      const ly = ny > cy ? ny + labelGap : ny - labelGap;
      ctx.fillText(node.id, nx, ly);
    });

    // Center label
    ctx.fillStyle = colors.text;
    ctx.font = 'bold 14px Inter, sans-serif';
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText(`${nodes.length}`, cx, cy - 6);
    ctx.fillStyle = colors.textMuted;
    ctx.font = '10px Inter, sans-serif';
    ctx.fillText('services', cx, cy + 8);

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
    <div className="card h-[294px]">
      <h3 className="text-sm font-semibold mb-2" style={{ color: 'rgb(var(--color-text))' }}>Cluster Topology</h3>
      <div ref={containerRef} className="w-full" style={{ height: 180 }}>
        <canvas ref={canvasRef} className="w-full h-full" style={{ height: 180 }} />
      </div>
      <div className="flex gap-3 mt-2 justify-center flex-wrap">
        {Object.entries(ROLE_COLORS)
          .filter(([role]) => role !== 'unknown')
          .map(([role, color]) => (
            <div key={role} className="flex items-center gap-1.5">
              <span
                className="inline-block w-2 h-2 rounded-full"
                style={{ backgroundColor: color }}
              />
              <span className="text-xs capitalize" style={{ color: 'rgb(var(--color-text-muted))' }}>{role}</span>
            </div>
          ))}
      </div>
    </div>
  );
}
