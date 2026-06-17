import { useState, useRef, useEffect, useMemo } from 'react';
import { useDashboard } from '../state/DashboardContext';
import { useQueryExecutor } from '../hooks/useQueryExecutor';

const EXAMPLE_QUERIES = [
  'cpu_usage_percent',
  'rate(http_requests_total[5m])',
  'avg by (host)(cpu_usage_percent)',
  'memory_usage_bytes{host="web-01"}',
  'sum(http_requests_total)',
];

const LISTBOX_ID = 'query-suggestions';
const optionId = (i: number) => `query-suggestion-${i}`;

export function QueryEditor() {
  const { state, dispatch } = useDashboard();
  const { execute, loading } = useQueryExecutor();
  const [input, setInput] = useState(state.query);
  const [open, setOpen] = useState(false);
  const [activeIndex, setActiveIndex] = useState(-1);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLUListElement>(null);

  useEffect(() => {
    setInput(state.query);
  }, [state.query]);

  // Examples narrow as the viewer types, so the menu is a real autocomplete
  // rather than a fixed list shown only on an empty field.
  const suggestions = useMemo(() => {
    const q = input.trim().toLowerCase();
    if (q === '') return EXAMPLE_QUERIES;
    return EXAMPLE_QUERIES.filter((s) => s.toLowerCase().includes(q));
  }, [input]);

  const listboxOpen = open && suggestions.length > 0;

  // Keep the active option scrolled into view during keyboard navigation.
  useEffect(() => {
    if (!listboxOpen || activeIndex < 0) return;
    document.getElementById(optionId(activeIndex))?.scrollIntoView({ block: 'nearest' });
  }, [activeIndex, listboxOpen]);

  const runQuery = (q: string) => {
    dispatch({ type: 'SET_QUERY', query: q });
    execute(q);
    setOpen(false);
    setActiveIndex(-1);
  };

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    runQuery(input);
  };

  const selectSuggestion = (q: string) => {
    setInput(q);
    runQuery(q);
    inputRef.current?.focus();
  };

  const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      if (!listboxOpen) {
        setOpen(true);
        setActiveIndex(0);
      } else {
        setActiveIndex((i) => Math.min(i + 1, suggestions.length - 1));
      }
    } else if (e.key === 'ArrowUp') {
      if (!listboxOpen) return;
      e.preventDefault();
      setActiveIndex((i) => Math.max(i - 1, 0));
    } else if (e.key === 'Enter') {
      if (listboxOpen && activeIndex >= 0) {
        // Choose the highlighted suggestion instead of submitting raw input.
        e.preventDefault();
        selectSuggestion(suggestions[activeIndex]);
      }
      // Otherwise fall through to the form's submit handler.
    } else if (e.key === 'Escape') {
      if (listboxOpen) {
        e.preventDefault();
        setOpen(false);
        setActiveIndex(-1);
      }
    }
  };

  return (
    <div className="relative">
      <form onSubmit={handleSubmit} className="flex gap-2">
        <div className="relative flex-1">
          <input
            ref={inputRef}
            type="text"
            role="combobox"
            aria-label="PromQL query"
            aria-expanded={listboxOpen}
            aria-controls={LISTBOX_ID}
            aria-autocomplete="list"
            aria-activedescendant={listboxOpen && activeIndex >= 0 ? optionId(activeIndex) : undefined}
            value={input}
            onChange={(e) => {
              setInput(e.target.value);
              setOpen(true);
              setActiveIndex(-1);
            }}
            onFocus={() => setOpen(true)}
            onBlur={() => setTimeout(() => setOpen(false), 150)}
            onKeyDown={handleKeyDown}
            placeholder="Enter a PromQL query — e.g. rate(http_requests_total[5m])"
            className="input w-full font-mono text-sm"
            spellCheck={false}
          />
          {listboxOpen && (
            <ul
              ref={listRef}
              id={LISTBOX_ID}
              role="listbox"
              aria-label="Example queries"
              className="absolute top-full left-0 right-0 mt-1 z-50 rounded-md border bg-surface py-1 max-h-64 overflow-y-auto"
            >
              {suggestions.map((q, i) => (
                <li
                  key={q}
                  id={optionId(i)}
                  role="option"
                  aria-selected={i === activeIndex}
                  // onMouseDown (not onClick) fires before the input's blur.
                  onMouseDown={(e) => {
                    e.preventDefault();
                    selectSuggestion(q);
                  }}
                  onMouseEnter={() => setActiveIndex(i)}
                  className={`cursor-pointer px-3 py-2 font-mono text-sm transition-colors ${
                    i === activeIndex ? 'bg-accent/15 text-accent' : 'text-text'
                  }`}
                >
                  {q}
                </li>
              ))}
            </ul>
          )}
        </div>
        <button
          type="submit"
          disabled={loading || !input.trim()}
          aria-busy={loading}
          aria-label={loading ? 'Running query' : 'Execute query'}
          className="btn-primary flex items-center gap-2 disabled:opacity-50"
        >
          {loading ? (
            <span className="inline-block w-4 h-4 border-2 border-white/30 border-t-white rounded-full motion-safe:animate-spin" />
          ) : (
            <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" aria-hidden="true">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M14 5l7 7m0 0l-7 7m7-7H3" />
            </svg>
          )}
          Execute
        </button>
      </form>

      {state.queryError && (
        <div
          role="alert"
          className="mt-2 px-3 py-2 rounded-md text-sm bg-crit/10 border border-crit/30 text-crit"
        >
          <span className="font-medium">Query failed:</span> {state.queryError}
        </div>
      )}
    </div>
  );
}
