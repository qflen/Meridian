import { useCallback } from 'react';
import { useDashboard } from '../state/DashboardContext';
import { useWebSocket } from './useWebSocket';
import { WSMessage } from '../types';

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
      }
    },
    [dispatch],
  );

  const handleConnect = useCallback(() => {
    dispatch({ type: 'SET_CONNECTED', connected: true });
  }, [dispatch]);

  const handleDisconnect = useCallback(() => {
    dispatch({ type: 'SET_CONNECTED', connected: false });
  }, [dispatch]);

  return useWebSocket('/ws/metrics', handleMessage, handleConnect, handleDisconnect);
}
