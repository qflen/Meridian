import { DashboardState, DashboardAction, Sample } from '../types';

const MAX_LIVE_SAMPLES = 300;

export const initialState: DashboardState = {
  theme: 'dark',
  timeRange: {
    start: Date.now() - 15 * 60 * 1000,
    end: Date.now(),
  },
  refreshInterval: 5000,
  query: '',
  queryResult: null,
  queryError: null,
  queryLoading: false,
  stats: null,
  liveMetrics: new Map(),
  clusterNodes: [],
  connected: false,
};

export function dashboardReducer(
  state: DashboardState,
  action: DashboardAction,
): DashboardState {
  switch (action.type) {
    case 'SET_THEME':
      return { ...state, theme: action.theme };

    case 'SET_TIME_RANGE':
      return { ...state, timeRange: action.range };

    case 'SET_REFRESH_INTERVAL':
      return { ...state, refreshInterval: action.interval };

    case 'SET_QUERY':
      return { ...state, query: action.query };

    case 'QUERY_START':
      return { ...state, queryLoading: true, queryError: null };

    case 'QUERY_SUCCESS':
      return { ...state, queryLoading: false, queryResult: action.result };

    case 'QUERY_ERROR':
      return { ...state, queryLoading: false, queryError: action.error };

    case 'SET_STATS':
      return { ...state, stats: action.stats };

    case 'ADD_LIVE_METRIC': {
      const next = new Map(state.liveMetrics);
      const existing = next.get(action.key) || [];
      const updated: Sample[] = [...existing, action.sample];
      if (updated.length > MAX_LIVE_SAMPLES) {
        updated.splice(0, updated.length - MAX_LIVE_SAMPLES);
      }
      next.set(action.key, updated);
      return { ...state, liveMetrics: next };
    }

    case 'SET_CLUSTER_NODES':
      return { ...state, clusterNodes: action.nodes };

    case 'SET_CONNECTED':
      return { ...state, connected: action.connected };

    default:
      return state;
  }
}
