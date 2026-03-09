import { DashboardState, DashboardAction, Sample, Anomaly } from '../types';

const MAX_LIVE_SAMPLES = 300;
const MAX_ANOMALIES = 50;

export const initialState: DashboardState = {
  theme: 'dark',
  query: '',
  queryResult: null,
  queryError: null,
  queryLoading: false,
  stats: null,
  liveMetrics: new Map(),
  clusterNodes: [],
  connectionStatus: 'connecting',
  anomalies: [],
  anomalyModel: '',
};

/**
 * Merge one anomaly into the list keyed by series, so the strip shows one row per
 * series carrying its latest transition (a `firing` then later a `resolved` event
 * update the same row). `seq` is monotonic, so a lower-or-equal seq is a stale or
 * duplicate frame (e.g. the seed re-delivering a live event) and is ignored. The
 * result is ordered most-recent-first and bounded.
 */
function upsertAnomaly(list: Anomaly[], a: Anomaly): Anomaly[] {
  const existing = list.find((x) => x.series === a.series);
  if (existing && existing.seq >= a.seq) return list;
  const next = list.filter((x) => x.series !== a.series);
  next.push(a);
  next.sort((x, y) => y.seq - x.seq);
  return next.length > MAX_ANOMALIES ? next.slice(0, MAX_ANOMALIES) : next;
}

export function dashboardReducer(
  state: DashboardState,
  action: DashboardAction,
): DashboardState {
  switch (action.type) {
    case 'SET_THEME':
      return { ...state, theme: action.theme };

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

    case 'SET_CONNECTION_STATUS':
      return { ...state, connectionStatus: action.status };

    case 'ADD_ANOMALY':
      return { ...state, anomalies: upsertAnomaly(state.anomalies, action.anomaly) };

    case 'SEED_ANOMALIES':
      // Fold the seed through the same upsert so live frames that already arrived
      // are never clobbered by an older buffered event. The seed also reports the
      // active detector model; keep the current value if this seed omits it.
      return {
        ...state,
        anomalies: action.anomalies.reduce(upsertAnomaly, state.anomalies),
        anomalyModel: action.model ?? state.anomalyModel,
      };

    default:
      return state;
  }
}
