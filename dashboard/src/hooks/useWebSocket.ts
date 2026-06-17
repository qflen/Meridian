import { useEffect, useRef, useCallback } from 'react';
import { WSMessage, ConnectionStatus } from '../types';
import { parseWSMessage } from '../utils/wsMessage';
import { nextReconnectDelay } from '../utils/backoff';

type MessageHandler = (msg: WSMessage) => void;
type StatusHandler = (status: ConnectionStatus) => void;

export function useWebSocket(
  path: string,
  onMessage: MessageHandler,
  onStatusChange?: StatusHandler,
) {
  const wsRef = useRef<WebSocket | null>(null);
  const bufferRef = useRef<WSMessage[]>([]);
  const rafRef = useRef(0);
  const handlersRef = useRef({ onMessage, onStatusChange });
  handlersRef.current = { onMessage, onStatusChange };

  useEffect(() => {
    // `disposed` makes the effect cleanup authoritative: once it runs, no
    // pending timer, frame, or in-flight socket may resurrect the connection
    // or dispatch into an unmounted tree.
    let disposed = false;
    let attempt = 0;
    let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
    let ws: WebSocket | null = null;

    // Drain the buffer once per frame, but only while messages are pending.
    // An empty buffer schedules nothing, so an idle socket costs zero frames.
    const flush = () => {
      rafRef.current = 0;
      const msgs = bufferRef.current;
      if (msgs.length === 0) return;
      bufferRef.current = [];
      for (const msg of msgs) handlersRef.current.onMessage(msg);
    };
    const scheduleFlush = () => {
      if (rafRef.current === 0) rafRef.current = requestAnimationFrame(flush);
    };

    const setStatus = (status: ConnectionStatus) => {
      if (!disposed) handlersRef.current.onStatusChange?.(status);
    };

    const scheduleReconnect = () => {
      if (disposed) return;
      const delay = nextReconnectDelay(attempt);
      attempt++;
      reconnectTimer = setTimeout(connect, delay);
    };

    function connect() {
      if (disposed) return;
      const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
      const url = `${protocol}//${window.location.host}${path}`;
      const sock = new WebSocket(url);
      ws = sock;
      wsRef.current = sock;

      sock.onopen = () => {
        if (disposed) return;
        attempt = 0; // reset backoff on a successful connection
        setStatus('connected');
      };

      sock.onmessage = (event) => {
        if (disposed) return;
        let raw: unknown;
        try {
          raw = JSON.parse(event.data);
        } catch {
          return; // ignore malformed JSON
        }
        const msg = parseWSMessage(raw);
        if (!msg) return; // ignore payloads that fail validation
        bufferRef.current.push(msg);
        scheduleFlush();
      };

      sock.onerror = () => {
        // An error is always followed by a close event; closing here makes the
        // transition into the reconnect path deterministic.
        sock.close();
      };

      sock.onclose = () => {
        if (disposed) return;
        if (wsRef.current === sock) wsRef.current = null;
        setStatus('reconnecting');
        scheduleReconnect();
      };
    }

    setStatus('connecting');
    connect();

    return () => {
      disposed = true;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      if (rafRef.current !== 0) {
        cancelAnimationFrame(rafRef.current);
        rafRef.current = 0;
      }
      bufferRef.current = [];
      if (ws) {
        // Detach handlers before closing so a late close/error from this socket
        // cannot fire after the effect has torn down.
        ws.onopen = ws.onmessage = ws.onerror = ws.onclose = null;
        ws.close();
      }
      wsRef.current = null;
    };
  }, [path]);

  const send = useCallback((data: unknown) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify(data));
    }
  }, []);

  return { send };
}
