import { useState, useRef, useEffect } from 'react';
import { useDashboard } from '../state/DashboardContext';
import { useQueryExecutor } from '../hooks/useQueryExecutor';

const EXAMPLE_QUERIES = [
  'cpu_usage_percent',
  'rate(http_requests_total[5m])',
  'avg by (host)(cpu_usage_percent)',
  'memory_usage_bytes{host="web-01"}',
  'sum(http_requests_total)',
];

export function QueryEditor() {
  const { state, dispatch } = useDashboard();
  const { execute, loading } = useQueryExecutor();
  const [input, setInput] = useState(state.query);
  const [showSuggestions, setShowSuggestions] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    setInput(state.query);
  }, [state.query]);

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    dispatch({ type: 'SET_QUERY', query: input });
    execute(input);
    setShowSuggestions(false);
  };

  const selectSuggestion = (q: string) => {
    setInput(q);
    dispatch({ type: 'SET_QUERY', query: q });
    setShowSuggestions(false);
    execute(q);
  };

  return (
    <div className="relative">
      <form onSubmit={handleSubmit} className="flex gap-2">
        <div className="relative flex-1">
          <input
            ref={inputRef}
            type="text"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onFocus={() => setShowSuggestions(true)}
            onBlur={() => setTimeout(() => setShowSuggestions(false), 200)}
            placeholder="Enter PromQL query... e.g. rate(http_requests_total[5m])"
            className="input w-full font-mono text-sm"
            spellCheck={false}
          />
          {showSuggestions && input === '' && (
            <div
              className="absolute top-full left-0 right-0 mt-1 z-50 rounded-lg shadow-xl"
              style={{
                border: '1px solid rgb(var(--color-border))',
                background: 'rgb(var(--color-surface))',
              }}
            >
              <div
                className="px-3 py-1.5 text-xs"
                style={{
                  color: 'rgb(var(--color-text-muted))',
                  borderBottom: '1px solid rgb(var(--color-border))',
                }}
              >
                Example queries
              </div>
              {EXAMPLE_QUERIES.map((q) => (
                <button
                  key={q}
                  type="button"
                  onMouseDown={() => selectSuggestion(q)}
                  className="block w-full text-left px-3 py-2 font-mono text-sm transition-colors"
                  style={{ color: 'rgb(var(--color-text))' }}
                  onMouseEnter={(e) => e.currentTarget.style.background = 'rgb(var(--color-text) / 0.06)'}
                  onMouseLeave={(e) => e.currentTarget.style.background = 'transparent'}
                >
                  {q}
                </button>
              ))}
            </div>
          )}
        </div>
        <button
          type="submit"
          disabled={loading || !input.trim()}
          className="btn-primary flex items-center gap-2 disabled:opacity-50"
        >
          {loading ? (
            <span className="inline-block w-4 h-4 border-2 border-white/30 border-t-white rounded-full animate-spin" />
          ) : (
            <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M14 5l7 7m0 0l-7 7m7-7H3" />
            </svg>
          )}
          Execute
        </button>
      </form>

      {state.queryError && (
        <div
          className="mt-2 px-3 py-2 rounded-lg text-sm"
          style={{
            background: 'rgb(var(--color-danger) / 0.1)',
            border: '1px solid rgb(var(--color-danger) / 0.3)',
            color: 'rgb(var(--color-danger))',
          }}
        >
          {state.queryError}
        </div>
      )}
    </div>
  );
}
