/**
 * Single source of truth for canvas text fonts.
 *
 * Canvas cannot read a CSS font stack, so the loaded families are mirrored here
 * and every `ctx.font` is built through `canvasFont()`. Numeric and axis text
 * uses the mono — the same IBM Plex Mono the DOM readouts use — so figures align
 * like an instrument; word labels (titles, legends, prompts) use the body sans.
 * Keep these stacks in sync with the families loaded in `main.tsx` and the
 * `fontFamily` definitions in `tailwind.config.ts`.
 */
export const CANVAS_MONO =
  "'IBM Plex Mono', ui-monospace, SFMono-Regular, Menlo, monospace";
export const CANVAS_SANS = 'Inter, system-ui, -apple-system, sans-serif';

interface CanvasFontOpts {
  weight?: number | string;
  /** 'mono' (default) for figures and axes, 'sans' for word labels. */
  family?: 'mono' | 'sans';
}

export function canvasFont(sizePx: number, opts: CanvasFontOpts = {}): string {
  const { weight = 400, family = 'mono' } = opts;
  const stack = family === 'sans' ? CANVAS_SANS : CANVAS_MONO;
  return `${weight} ${sizePx}px ${stack}`;
}
