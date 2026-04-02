import { useDashboard } from '../state/DashboardContext';
import { Theme } from '../types';

const THEMES: { value: Theme; label: string; icon: string }[] = [
  { value: 'dark', label: 'Dark', icon: 'M20.354 15.354A9 9 0 018.646 3.646 9.003 9.003 0 0012 21a9.003 9.003 0 008.354-5.646z' },
  { value: 'light', label: 'Light', icon: 'M12 3v1m0 16v1m9-9h-1M4 12H3m15.364 6.364l-.707-.707M6.343 6.343l-.707-.707m12.728 0l-.707.707M6.343 17.657l-.707.707M16 12a4 4 0 11-8 0 4 4 0 018 0z' },
  { value: 'high-contrast', label: 'HC', icon: 'M12 3v1m0 16v1m9-9h-1M4 12H3m3.343-5.657L7.05 7.05m9.9 9.9l.707.707M12 8a4 4 0 100 8 4 4 0 000-8z' },
];

export function ThemeToggle() {
  const { state, dispatch } = useDashboard();

  const setTheme = (theme: Theme) => {
    dispatch({ type: 'SET_THEME', theme });
    const root = document.documentElement;
    root.classList.remove('dark', 'light', 'high-contrast');
    root.classList.add(theme);
  };

  return (
    <div className="flex items-center gap-1 rounded-lg border border-gray-700 p-0.5">
      {THEMES.map((t) => (
        <button
          key={t.value}
          onClick={() => setTheme(t.value)}
          title={t.label}
          className={`rounded-md p-1.5 transition-colors ${
            state.theme === t.value
              ? 'bg-meridian-600 text-white'
              : 'text-gray-500 hover:text-gray-300'
          }`}
        >
          <svg className="w-3.5 h-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d={t.icon} />
          </svg>
        </button>
      ))}
    </div>
  );
}
