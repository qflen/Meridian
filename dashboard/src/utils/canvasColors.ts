/**
 * Resolve CSS custom properties for use in a Canvas 2D context.
 * Canvas does NOT support CSS variables — `ctx.fillStyle = 'rgb(var(--x))'`
 * silently fails to black — so we read the computed channel triples and build
 * concrete color strings. The graticule colors are derived from the single
 * border token so the canvas grid and the DOM hairlines stay in one system.
 */
export interface CanvasColors {
  text: string;
  textMuted: string;
  border: string;
  /** Panel surface — used to ring markers so they read on top of a trace. */
  surface: string;
  accent: string;
  success: string;
  warning: string;
  danger: string;
  /** Faint graticule lines (border token at reduced alpha). */
  gridColor: string;
  /** Plot frame / stronger ticks. */
  gridStrong: string;
  /** Minor graticule subdivisions — fainter than gridColor. */
  gridFaint: string;
}

export function getCanvasColors(el: HTMLElement): CanvasColors {
  const style = getComputedStyle(el);
  const get = (name: string) => style.getPropertyValue(name).trim();

  const channels = (name: string) => get(name).split(/\s+/);
  const rgb = (name: string) => {
    const [r, g, b] = channels(name);
    return `rgb(${r} ${g} ${b})`;
  };
  const rgba = (name: string, a: number) => {
    const [r, g, b] = channels(name);
    return `rgba(${r}, ${g}, ${b}, ${a})`;
  };

  return {
    text: rgb('--color-text'),
    textMuted: rgb('--color-text-muted'),
    border: rgb('--color-border'),
    surface: rgb('--color-surface'),
    accent: rgb('--color-accent'),
    success: rgb('--color-success'),
    warning: rgb('--color-warning'),
    danger: rgb('--color-danger'),
    gridColor: rgba('--color-border', 0.5),
    gridStrong: rgba('--color-border', 0.9),
    gridFaint: rgba('--color-border', 0.22),
  };
}
