import { describe, it, expect } from 'vitest';
import { dashboardReducer, initialState } from './dashboardReducer';
import { Anomaly, DashboardState } from '../types';

function anomaly(over: Partial<Anomaly>): Anomaly {
  return {
    seq: 1,
    series: 's',
    metric: 'm',
    labels: {},
    value: 100,
    baseline: 50,
    score: 5,
    severity: 'warn',
    state: 'firing',
    timestamp: 1000,
    ...over,
  };
}

function withAnomalies(anomalies: Anomaly[]): DashboardState {
  return { ...initialState, anomalies };
}

describe('dashboardReducer anomalies', () => {
  it('ADD_ANOMALY keeps one row per series, latest transition winning', () => {
    let state = withAnomalies([]);
    state = dashboardReducer(state, { type: 'ADD_ANOMALY', anomaly: anomaly({ seq: 1, state: 'firing' }) });
    expect(state.anomalies).toHaveLength(1);
    // A later resolved event on the same series updates the same row, not a new one.
    state = dashboardReducer(state, { type: 'ADD_ANOMALY', anomaly: anomaly({ seq: 4, state: 'resolved' }) });
    expect(state.anomalies).toHaveLength(1);
    expect(state.anomalies[0].state).toBe('resolved');
    expect(state.anomalies[0].seq).toBe(4);
  });

  it('ADD_ANOMALY ignores a stale (lower-or-equal seq) frame', () => {
    let state = withAnomalies([anomaly({ seq: 5, state: 'resolved' })]);
    state = dashboardReducer(state, { type: 'ADD_ANOMALY', anomaly: anomaly({ seq: 3, state: 'firing' }) });
    expect(state.anomalies[0].seq).toBe(5);
    expect(state.anomalies[0].state).toBe('resolved');
  });

  it('orders multiple series most-recent (highest seq) first', () => {
    let state = withAnomalies([]);
    state = dashboardReducer(state, { type: 'ADD_ANOMALY', anomaly: anomaly({ series: 'a', seq: 1 }) });
    state = dashboardReducer(state, { type: 'ADD_ANOMALY', anomaly: anomaly({ series: 'b', seq: 9 }) });
    state = dashboardReducer(state, { type: 'ADD_ANOMALY', anomaly: anomaly({ series: 'c', seq: 4 }) });
    expect(state.anomalies.map((a) => a.series)).toEqual(['b', 'c', 'a']);
  });

  it('SEED_ANOMALIES merges without clobbering a newer live event', () => {
    // A live frame already raised series 's' at seq 10.
    let state = withAnomalies([anomaly({ series: 's', seq: 10, state: 'firing' })]);
    // The seed (most-recent-first) carries an older event for 's' and a new series.
    state = dashboardReducer(state, {
      type: 'SEED_ANOMALIES',
      anomalies: [anomaly({ series: 't', seq: 8 }), anomaly({ series: 's', seq: 2 })],
    });
    const s = state.anomalies.find((a) => a.series === 's');
    const t = state.anomalies.find((a) => a.series === 't');
    expect(s?.seq).toBe(10); // newer live event preserved
    expect(t?.seq).toBe(8); // new series added from the seed
  });
});
