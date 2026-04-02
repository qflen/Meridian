import { useEffect, useRef, useCallback } from 'react';
import { WSMessage } from '../types';

type MessageHandler = (msg: WSMessage) => void;

export function useWebSocket(
  path: string,
  onMessage: MessageHandler,
  onConnect?: () => void,
  onDisconnect?: () => void,
) {
  const wsRef = useRef<WebSocket | null>(null);
  const bufferRef = useRef<WSMessage[]>([]);
  const rafRef = useRef<number>(0);
  const handlersRef = useRef({ onMessage, onConnect, onDisconnect });
  handlersRef.current = { onMessage, onConnect, onDisconnect };

  // requestAnimationFrame batching per ADR requirement
  const flushBuffer = useCallback(() => {
    const msgs = bufferRef.current;
    if (msgs.length > 0) {
      bufferRef.current = [];
      for (const msg of msgs) {
        handlersRef.current.onMessage(msg);
      }
    }
    rafRef.current = requestAnimationFrame(flushBuffer);
  }, []);

  useEffect(() => {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const url = `${protocol}//${window.location.host}${path}`;
    const ws = new WebSocket(url);
    wsRef.current = ws;

    ws.onopen = () => {
      handlersRef.current.onConnect?.();
    };

    ws.onmessage = (event) => {
      try {
        const msg = JSON.parse(event.data) as WSMessage;
        bufferRef.current.push(msg);
      } catch {
        // ignore malformed messages
      }
    };

    ws.onclose = () => {
      handlersRef.current.onDisconnect?.();
    };

    rafRef.current = requestAnimationFrame(flushBuffer);

    return () => {
      cancelAnimationFrame(rafRef.current);
      ws.close();
    };
  }, [path, flushBuffer]);

  const send = useCallback((data: unknown) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify(data));
    }
  }, []);

  return { send };
}
