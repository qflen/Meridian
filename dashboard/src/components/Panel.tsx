import type { ReactNode } from 'react';

/**
 * Panel — the one instrument surface, in three deliberate tiers.
 *
 * Hierarchy is carried by size, heading scale, and density alone (never by
 * shadow or glow): a `primary` panel is full-width with a larger display
 * heading and a small muted eyebrow; `secondary` monitors read at body scale; and
 * `tertiary` readouts are dense, with a small engraved uppercase label. A fixed
 * `bodyHeight` (drawn from the shared `PANEL_BODY` scale) makes the body a flex
 * column so the chart or list inside fills it — that is where panels in a row
 * get their alignment, without anyone hard-coding a magic pixel height.
 */
export type PanelTier = 'primary' | 'secondary' | 'tertiary';

interface PanelProps {
  tier?: PanelTier;
  /** Heading text, rendered as an <h2> in the display face. */
  title?: ReactNode;
  /** Small uppercase kicker shown above a primary title. */
  eyebrow?: string;
  /** Right-aligned header content — counts, stats, status chips. */
  meta?: ReactNode;
  /** Fixed body height in px. The body becomes a flex column; fill with flex-1. */
  bodyHeight?: number;
  className?: string;
  bodyClassName?: string;
  children: ReactNode;
}

const PAD: Record<PanelTier, string> = {
  primary: 'p-4 sm:p-5',
  secondary: 'p-4',
  tertiary: 'p-3',
};

const TITLE: Record<PanelTier, string> = {
  primary: 'text-base font-semibold tracking-tight text-text',
  secondary: 'text-sm font-semibold text-text',
  tertiary: 'text-2xs font-semibold uppercase tracking-wider text-muted',
};

const HEADER_GAP: Record<PanelTier, string> = {
  primary: 'mb-3',
  secondary: 'mb-3',
  tertiary: 'mb-2',
};

export function Panel({
  tier = 'secondary',
  title,
  eyebrow,
  meta,
  bodyHeight,
  className = '',
  bodyClassName = '',
  children,
}: PanelProps) {
  const hasHeader = title != null || meta != null || eyebrow != null;
  return (
    <section className={`panel ${PAD[tier]} flex flex-col min-w-0 ${className}`}>
      {hasHeader && (
        <header className={`flex items-baseline justify-between gap-3 ${HEADER_GAP[tier]}`}>
          <div className="min-w-0">
            {eyebrow && (
              <div className="text-2xs font-medium uppercase tracking-wider text-muted mb-1">
                {eyebrow}
              </div>
            )}
            {title != null && <h2 className={`${TITLE[tier]} truncate`}>{title}</h2>}
          </div>
          {meta != null && (
            <div className="shrink-0 text-xs text-muted tabular-nums">{meta}</div>
          )}
        </header>
      )}
      <div
        className={`min-w-0 ${bodyHeight != null ? 'flex flex-col min-h-0' : ''} ${bodyClassName}`}
        style={bodyHeight != null ? { height: bodyHeight } : undefined}
      >
        {children}
      </div>
    </section>
  );
}
