/**
 * Resolve CSS custom properties for use in Canvas 2D context.
 * Canvas does NOT support CSS variables — ctx.fillStyle = 'rgb(var(--x))' silently fails to black.
 */
export function getCanvasColors(el: HTMLElement) {
  const style = getComputedStyle(el);
  const get = (name: string) => style.getPropertyValue(name).trim();
  return {
    text: `rgb(${get('--color-text')})`,
    textMuted: `rgb(${get('--color-text-muted')})`,
    border: `rgb(${get('--color-border')})`,
    accent: `rgb(${get('--color-accent')})`,
    success: `rgb(${get('--color-success')})`,
    warning: `rgb(${get('--color-warning')})`,
    danger: `rgb(${get('--color-danger')})`,
    gridColor: get('--grid-color') || 'rgba(128,128,128,0.2)',
    chartGlow: get('--chart-glow') || 'rgba(92,124,250,0.2)',
  };
}
