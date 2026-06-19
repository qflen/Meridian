import type { Config } from 'tailwindcss';

// Semantic colors bound to the CSS custom properties in index.css. Composing
// alpha through `<alpha-value>` lets utilities like `bg-text/5` or `border-crit/30`
// resolve against the same token, so there is one source of truth per theme.
const token = (name: string) => `rgb(var(${name}) / <alpha-value>)`;

export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  darkMode: 'class',
  theme: {
    extend: {
      colors: {
        // Named `page`, not `base`: a `base` colour would generate a `text-base`
        // colour utility that collides with Tailwind's `text-base` font size.
        page: token('--color-bg'),
        surface: token('--color-surface'),
        border: token('--color-border'),
        text: token('--color-text'),
        muted: token('--color-text-muted'),
        accent: token('--color-accent'),
        ok: token('--color-success'),
        warn: token('--color-warning'),
        crit: token('--color-danger'),
      },
      // A plain `border` defaults to the hairline token instead of gray-200.
      borderColor: {
        DEFAULT: 'rgb(var(--color-border))',
      },
      fontFamily: {
        display: ['"Inter Tight"', 'Inter', 'system-ui', 'sans-serif'],
        sans: ['Inter', 'system-ui', '-apple-system', 'BlinkMacSystemFont', 'sans-serif'],
        mono: ['"IBM Plex Mono"', 'ui-monospace', 'SFMono-Regular', 'Menlo', 'monospace'],
      },
      fontSize: {
        '2xs': ['0.6875rem', { lineHeight: '1rem' }], // 11px micro-labels
      },
    },
  },
  plugins: [],
} satisfies Config;
