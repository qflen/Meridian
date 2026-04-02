// ── Data types ──────────────────────────────────────────────────────

export interface Sample {
  timestamp: number;
  value: number;
}

export interface TimeSeries {
  labels: Record<string, string>;
  samples: Sample[];
}

export interface QueryResult {
  status: string;
  data: TimeSeries[];
  stats?: QueryStats;
}

export interface QueryStats {
  seriesFetched: number;
  samplesFetched: number;
  executionMs: number;
}

// ── WebSocket messages ──────────────────────────────────────────────

export interface WSMetricMessage {
  type: 'metric';
  series: string;
  labels: Record<string, string>;
  timestamp: number;
  value: number;
}

export interface WSLiveMessage {
  type: 'live';
  series: TimeSeries[];
}

export interface WSStatsMessage {
  type: 'stats';
  ingestionRate: number;
  activeSeries: number;
  memoryBytes: number;
  compressedBytes: number;
  rawBytes: number;
  walSegments: number;
  blockCount: number;
  uptimeSeconds: number;
}

export type WSMessage = WSMetricMessage | WSLiveMessage | WSStatsMessage;

// ── Cluster types ───────────────────────────────────────────────────

export interface ClusterNode {
  id: string;
  address: string;
  state: 'joining' | 'active' | 'leaving' | 'dead';
  series: number;
  samples: number;
}

// ── Dashboard state ─────────────────────────────────────────────────

export type Theme = 'dark' | 'light' | 'high-contrast';

export interface TimeRange {
  start: number;
  end: number;
}

export interface DashboardState {
  theme: Theme;
  timeRange: TimeRange;
  refreshInterval: number;
  query: string;
  queryResult: QueryResult | null;
  queryError: string | null;
  queryLoading: boolean;
  stats: WSStatsMessage | null;
  liveMetrics: Map<string, Sample[]>;
  clusterNodes: ClusterNode[];
  connected: boolean;
}

export type DashboardAction =
  | { type: 'SET_THEME'; theme: Theme }
  | { type: 'SET_TIME_RANGE'; range: TimeRange }
  | { type: 'SET_REFRESH_INTERVAL'; interval: number }
  | { type: 'SET_QUERY'; query: string }
  | { type: 'QUERY_START' }
  | { type: 'QUERY_SUCCESS'; result: QueryResult }
  | { type: 'QUERY_ERROR'; error: string }
  | { type: 'SET_STATS'; stats: WSStatsMessage }
  | { type: 'ADD_LIVE_METRIC'; key: string; sample: Sample }
  | { type: 'SET_CLUSTER_NODES'; nodes: ClusterNode[] }
  | { type: 'SET_CONNECTED'; connected: boolean };
