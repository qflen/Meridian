import { useCallback } from 'react';
import { useDashboard } from '../state/DashboardContext';
import { QueryResult, TimeSeries } from '../types';

const DEFAULT_LOOKBACK_MS = 15 * 60 * 1000;

export function useQueryExecutor() {
  const { state, dispatch } = useDashboard();

  const execute = useCallback(
    async (query?: string) => {
      const q = query ?? state.query;
      if (!q.trim()) return;

      dispatch({ type: 'QUERY_START' });

      try {
        // Resolve the range at execute-time so queries always cover the most recent samples,
        // even if the app has been open for a while. A fixed state-bound range would go stale.
        const now = Date.now();
        const end = now;
        const start = now - DEFAULT_LOOKBACK_MS;

        const params = new URLSearchParams({
          q,
          start: String(start),
          end: String(end),
        });

        const res = await fetch(`/api/v1/query?${params}`);
        if (!res.ok) {
          const text = await res.text();
          // Try to parse as JSON error
          try {
            const errObj = JSON.parse(text);
            throw new Error(errObj.error || text);
          } catch {
            throw new Error(text || `HTTP ${res.status}`);
          }
        }

        const raw = await res.json();

        // Adapt server response format to dashboard QueryResult
        // Server: { data: { result: [{ name, labels, values: [[ts, val]] }] }, exec_time }
        const serverResult = raw.data?.result ?? raw.data ?? [];
        const data: TimeSeries[] = Array.isArray(serverResult)
          ? serverResult.map((r: { name?: string; labels?: Record<string, string>; values?: number[][] }) => ({
              labels: { __name__: r.name ?? '', ...(r.labels ?? {}) },
              samples: (r.values ?? []).map((v: number[]) => ({
                timestamp: v[0],
                value: v[1],
              })),
            }))
          : [];

        const result: QueryResult = {
          status: raw.status ?? 'success',
          data,
          stats: {
            seriesFetched: data.length,
            samplesFetched: data.reduce((n, s) => n + s.samples.length, 0),
            executionMs: parseFloat(raw.exec_time) || 0,
          },
        };
        dispatch({ type: 'QUERY_SUCCESS', result });
      } catch (err) {
        dispatch({
          type: 'QUERY_ERROR',
          error: err instanceof Error ? err.message : 'Query failed',
        });
      }
    },
    [state.query, dispatch],
  );

  return { execute, loading: state.queryLoading };
}
