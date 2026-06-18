import { useCallback } from 'react';
import { useDashboard } from '../state/DashboardContext';
import { useWebSocket } from './useWebSocket';
import { WSMessage, ConnectionStatus } from '../types';

export function useMetricStream() {
  const { dispatch } = useDashboard();

  const handleMessage = useCallback(
    (msg: WSMessage) => {
      switch (msg.type) {
        case 'metric':
          dispatch({
            type: 'ADD_LIVE_METRIC',
            key: msg.series,
            sample: { timestamp: msg.timestamp, value: msg.value },
          });
          break;
        case 'stats':
          dispatch({ type: 'SET_STATS', stats: msg });
          break;
        case 'anomaly':
          // WSAnomalyMessage carries every Anomaly field (plus the frame `type`).
          dispatch({ type: 'ADD_ANOMALY', anomaly: msg });
          break;
      }
    },
    [dispatch],
  );

  const handleStatus = useCallback(
    (status: ConnectionStatus) => {
      dispatch({ type: 'SET_CONNECTION_STATUS', status });
    },
    [dispatch],
  );

  return useWebSocket('/ws/metrics', handleMessage, handleStatus);
}
