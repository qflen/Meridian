import { useRef, useEffect, useCallback } from 'react';
import { ClusterNode } from '../types';
import { useDashboard } from '../state/DashboardContext';
import { getCanvasColors } from '../utils/canvasColors';
import { canvasFont } from '../utils/canvasFont';
import { CATEGORICAL } from '../utils/chartPalette';

// Roles draw from the shared muted categorical palette; node STATE
// (active/dead/…) is colored from the status tokens inside render() so it
// tracks the theme.
const ROLE_COLORS: Record<string, string> = {
  gateway: CATEGORICAL[1],   // slate blue
  ingestor: CATEGORICAL[0],  // brass amber
  storage: CATEGORICAL[2],   // sage green
  querier: CATEGORICAL[3],   // muted orchid
  compactor: CATEGORICAL[4], // clay
  unknown: '#8A8F99',
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
  const rafRef = useRef(0);
  const nodesRef = useRef<ClusterNode[]>([]);

  const nodes: ClusterNode[] = state.clusterNodes;
  nodesRef.current = nodes;
  const theme = state.theme;

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

  // Pure paint pass. Reads the current nodes via a ref so it has no reactive
  // dependencies and never schedules a follow-up frame — the topology is a
  // static diagram, repainted on data/theme/resize, not animated.
  const render = useCallback(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const ctx = canvas.getContext('2d');
    if (!ctx) return;
    const nodes = nodesRef.current;

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
        const roleColor = ROLE_COLORS[other.role] || ROLE_COLORS.unknown;
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
      const stateColor =
        node.state === 'active'
          ? colors.success
          : node.state === 'dead'
            ? colors.danger
            : colors.warning; // joining / leaving
      const roleColor = ROLE_COLORS[node.role] || ROLE_COLORS.unknown;
      const nodeR = node.state === 'active' ? 10 : 7;

      // Node circle: role color when active, status color otherwise
      ctx.fillStyle = node.state === 'active' ? roleColor : stateColor;
      ctx.beginPath();
      ctx.arc(nx, ny, nodeR, 0, Math.PI * 2);
      ctx.fill();

      // Hairline border
      ctx.strokeStyle = colors.border;
      ctx.lineWidth = 1;
      ctx.stroke();

      // Role abbreviation inside the circle
      ctx.fillStyle = '#fff';
      ctx.font = canvasFont(nodeR > 8 ? 8 : 7, { weight: 600 });
      ctx.textAlign = 'center';
      ctx.textBaseline = 'middle';
      ctx.fillText(ROLE_ICONS[node.role] || '??', nx, ny);

      // Label below/above
      ctx.fillStyle = colors.textMuted;
      ctx.font = canvasFont(10, { family: 'sans' });
      ctx.textAlign = 'center';
      ctx.textBaseline = 'alphabetic';
      const labelGap = nodeR + 14;
      const ly = ny > cy ? ny + labelGap : ny - labelGap;
      ctx.fillText(node.id, nx, ly);
    });

    // Center label
    ctx.fillStyle = colors.text;
    ctx.font = canvasFont(14, { weight: 600 });
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText(`${nodes.length}`, cx, cy - 6);
    ctx.fillStyle = colors.textMuted;
    ctx.font = canvasFont(10, { family: 'sans' });
    ctx.fillText('services', cx, cy + 8);
  }, []);

  // Single coalesced rAF driver: render() never schedules itself, so a repaint
  // is requested here and at most one frame is ever in flight.
  const scheduleRender = useCallback(() => {
    if (rafRef.current !== 0) return;
    rafRef.current = requestAnimationFrame(() => {
      rafRef.current = 0;
      render();
    });
  }, [render]);

  // Repaint when the cluster data or the theme changes.
  useEffect(() => {
    scheduleRender();
  }, [nodes, theme, scheduleRender]);

  // Repaint on resize. The observer only requests a frame; it must not call
  // render() directly, which previously started a second concurrent rAF loop
  // on every resize/theme toggle.
  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;
    const ro = new ResizeObserver(() => scheduleRender());
    ro.observe(container);
    return () => ro.disconnect();
  }, [scheduleRender]);

  // Cancel any pending frame on unmount.
  useEffect(
    () => () => {
      if (rafRef.current !== 0) cancelAnimationFrame(rafRef.current);
    },
    [],
  );

  return (
    <div className="card h-[294px]">
      <h3 className="text-sm font-semibold mb-2">Cluster Topology</h3>
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
              <span className="text-xs capitalize text-muted">{role}</span>
            </div>
          ))}
      </div>
    </div>
  );
}
