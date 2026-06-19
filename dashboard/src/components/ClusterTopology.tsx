import { useRef, useEffect, useCallback } from 'react';
import { ClusterNode } from '../types';
import { useDashboard } from '../state/DashboardContext';
import { Panel } from './Panel';
import { Placeholder } from './Placeholder';
import { getCanvasColors } from '../utils/canvasColors';
import { canvasFont } from '../utils/canvasFont';
import { CATEGORICAL } from '../utils/chartPalette';
import { PANEL_BODY } from '../utils/layout';

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

// Each role reads as a distinct shape rather than a two-letter code: gateway a
// diamond (routing), ingestor a down-triangle (intake), storage a square
// (block), querier a circle (lens), compactor a hexagon (merge). The legend
// (RoleGlyph) mirrors these exactly.
function nodeShapePath(
  ctx: CanvasRenderingContext2D,
  role: string,
  x: number,
  y: number,
  r: number,
): void {
  ctx.beginPath();
  switch (role) {
    case 'gateway': // diamond
      ctx.moveTo(x, y - r);
      ctx.lineTo(x + r, y);
      ctx.lineTo(x, y + r);
      ctx.lineTo(x - r, y);
      ctx.closePath();
      break;
    case 'ingestor': { // down-triangle
      const t = r * 1.08;
      ctx.moveTo(x - t, y - t * 0.7);
      ctx.lineTo(x + t, y - t * 0.7);
      ctx.lineTo(x, y + t);
      ctx.closePath();
      break;
    }
    case 'storage': { // square
      const s = r * 0.92;
      ctx.rect(x - s, y - s, s * 2, s * 2);
      break;
    }
    case 'compactor': // hexagon
      for (let i = 0; i < 6; i++) {
        const a = (Math.PI / 3) * i - Math.PI / 6;
        const px = x + Math.cos(a) * r;
        const py = y + Math.sin(a) * r;
        if (i === 0) ctx.moveTo(px, py);
        else ctx.lineTo(px, py);
      }
      ctx.closePath();
      break;
    case 'querier': // circle (lens)
      ctx.arc(x, y, r, 0, Math.PI * 2);
      break;
    default: // unknown — a smaller circle
      ctx.arc(x, y, r * 0.7, 0, Math.PI * 2);
  }
}

/** DOM mirror of nodeShapePath, used by the legend so it matches the canvas. */
function RoleGlyph({ role, color }: { role: string; color: string }) {
  const cls = 'w-2.5 h-2.5 shrink-0';
  switch (role) {
    case 'gateway':
      return <svg viewBox="0 0 16 16" className={cls} aria-hidden="true"><polygon points="8,1 15,8 8,15 1,8" fill={color} /></svg>;
    case 'ingestor':
      return <svg viewBox="0 0 16 16" className={cls} aria-hidden="true"><polygon points="2,4 14,4 8,15" fill={color} /></svg>;
    case 'storage':
      return <svg viewBox="0 0 16 16" className={cls} aria-hidden="true"><rect x="2" y="2" width="12" height="12" fill={color} /></svg>;
    case 'compactor':
      return <svg viewBox="0 0 16 16" className={cls} aria-hidden="true"><polygon points="13.6,4.8 13.6,11.2 8,14.5 2.4,11.2 2.4,4.8 8,1.5" fill={color} /></svg>;
    case 'querier':
      return <svg viewBox="0 0 16 16" className={cls} aria-hidden="true"><circle cx="8" cy="8" r="6.5" fill={color} /></svg>;
    default:
      return <svg viewBox="0 0 16 16" className={cls} aria-hidden="true"><circle cx="8" cy="8" r="4.5" fill={color} /></svg>;
  }
}

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
    const radius = Math.min(w, h) * 0.32;

    if (nodes.length === 0) return;

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
      const nodeR = node.state === 'active' ? 9 : 6.5;

      // Role shape, filled with the role colour when active or the status
      // colour otherwise, on a hairline border.
      nodeShapePath(ctx, node.role, nx, ny, nodeR);
      ctx.fillStyle = node.state === 'active' ? roleColor : stateColor;
      ctx.fill();
      ctx.strokeStyle = colors.border;
      ctx.lineWidth = 1;
      ctx.stroke();

      // Label below/above
      ctx.fillStyle = colors.textMuted;
      ctx.font = canvasFont(10, { family: 'sans' });
      ctx.textAlign = 'center';
      ctx.textBaseline = 'alphabetic';
      const labelGap = nodeR + 14;
      const ly = ny > cy ? ny + labelGap : ny - labelGap;
      ctx.fillText(node.id, nx, ly);
    });

    // Center label: the member count, correctly pluralised.
    ctx.fillStyle = colors.text;
    ctx.font = canvasFont(16, { weight: 500 });
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText(`${nodes.length}`, cx, cy - 7);
    ctx.fillStyle = colors.textMuted;
    ctx.font = canvasFont(10, { family: 'sans' });
    ctx.fillText(nodes.length === 1 ? 'node' : 'nodes', cx, cy + 9);
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

  // Legend only the roles actually present, in a fixed pipeline order, so a
  // single-binary node does not sit under a legend for five services.
  const presentRoles = Object.keys(ROLE_COLORS).filter(
    (role) => role !== 'unknown' && nodes.some((n) => n.role === role),
  );
  const active = nodes.filter((n) => n.state === 'active').length;
  const meta = nodes.length > 0 ? `${active} / ${nodes.length} active` : null;

  return (
    <Panel tier="secondary" title="Cluster Topology" meta={meta} bodyHeight={PANEL_BODY.monitor}>
      <div ref={containerRef} className="relative w-full flex-1 min-h-0">
        <canvas ref={canvasRef} className="block w-full h-full" />
        {nodes.length === 0 && (
          <div className="pointer-events-none absolute inset-0">
            <Placeholder title="No cluster members reported yet." hint="Nodes appear on the ring as they join." />
          </div>
        )}
      </div>
      {presentRoles.length > 0 && (
        <div className="flex gap-x-4 gap-y-1 mt-2 justify-center flex-wrap">
          {presentRoles.map((role) => (
            <div key={role} className="flex items-center gap-1.5">
              <RoleGlyph role={role} color={ROLE_COLORS[role]} />
              <span className="text-xs capitalize text-muted">{role}</span>
            </div>
          ))}
        </div>
      )}
    </Panel>
  );
}
