import { useCallback } from 'react';
import { useDashboard } from '../state/DashboardContext';
import { QueryResult } from '../types';

export function useQueryExecutor() {
  const { state, dispatch } = useDashboard();

  const execute = useCallback(
    async (query?: string) => {
      const q = query ?? state.query;
      if (!q.trim()) return;

      dispatch({ type: 'QUERY_START' });

      try {
        const params = new URLSearchParams({
          q,
          start: String(state.timeRange.start),
          end: String(state.timeRange.end),
          format: 'json',
        });

        const res = await fetch(`/api/query?${params}`);
        if (!res.ok) {
          const text = await res.text();
          throw new Error(text || `HTTP ${res.status}`);
        }

        const result: QueryResult = await res.json();
        dispatch({ type: 'QUERY_SUCCESS', result });
      } catch (err) {
        dispatch({
          type: 'QUERY_ERROR',
          error: err instanceof Error ? err.message : 'Query failed',
        });
      }
    },
    [state.query, state.timeRange, dispatch],
  );

  return { execute, loading: state.queryLoading };
}
