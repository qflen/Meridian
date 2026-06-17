import type { ReactNode } from 'react';

/**
 * One calm, instrument-style placeholder for every empty / loading / error
 * state, so the dashboard speaks with a single voice instead of four
 * differently-worded canvas captions. The copy is written from the viewer's
 * side of the screen — plain, active, specific — and an error always says what
 * happened and points at the fix.
 */
type PlaceholderKind = 'empty' | 'loading' | 'error';

interface PlaceholderProps {
  kind?: PlaceholderKind;
  title: string;
  hint?: ReactNode;
  className?: string;
}

export function Placeholder({ kind = 'empty', title, hint, className = '' }: PlaceholderProps) {
  return (
    <div
      role={kind === 'error' ? 'alert' : kind === 'loading' ? 'status' : undefined}
      className={`flex h-full w-full flex-col items-center justify-center gap-2 px-4 py-6 text-center ${className}`}
    >
      <Mark kind={kind} />
      <div className={`text-sm ${kind === 'error' ? 'text-crit' : 'text-text'}`}>{title}</div>
      {hint && <div className="max-w-xs text-xs leading-relaxed text-muted">{hint}</div>}
    </div>
  );
}

function Mark({ kind }: { kind: PlaceholderKind }) {
  if (kind === 'loading') {
    return (
      <span
        aria-hidden="true"
        className="h-4 w-4 rounded-full border-2 border-muted/30 border-t-accent motion-safe:animate-spin"
      />
    );
  }
  if (kind === 'error') {
    return (
      <svg
        aria-hidden="true"
        viewBox="0 0 24 24"
        className="h-5 w-5 text-crit"
        fill="none"
        stroke="currentColor"
        strokeWidth={1.75}
      >
        <path
          strokeLinecap="round"
          strokeLinejoin="round"
          d="M12 9v4m0 3.5h.01M10.3 4.3 2.6 18a2 2 0 0 0 1.7 3h15.4a2 2 0 0 0 1.7-3L13.7 4.3a2 2 0 0 0-3.4 0Z"
        />
      </svg>
    );
  }
  // Empty — a flat instrument baseline, the trace not yet drawn.
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 32 16"
      className="h-4 w-8 text-muted/60"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.5}
    >
      <path strokeLinecap="round" d="M1 12h7m16 0h7" />
      <path strokeLinecap="round" strokeDasharray="1 3" d="M8 12h16" />
    </svg>
  );
}
