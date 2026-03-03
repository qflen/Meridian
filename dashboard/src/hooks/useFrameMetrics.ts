import { useRef, useCallback, useEffect, useState } from 'react';
import {
  droppedFramesForDelta,
  MAX_PLAUSIBLE_FRAME_MS,
} from '../utils/frameMetrics';

interface FrameMetrics {
  fps: number;
  frameTime: number;
  droppedFrames: number;
}

export function useFrameMetrics(): FrameMetrics {
  const [metrics, setMetrics] = useState<FrameMetrics>({
    fps: 60,
    frameTime: 16.67,
    droppedFrames: 0,
  });

  const frameTimesRef = useRef<number[]>([]);
  const lastFrameRef = useRef(performance.now());
  const droppedRef = useRef(0);
  const rafRef = useRef(0);

  const measure = useCallback(() => {
    const now = performance.now();
    const delta = now - lastFrameRef.current;
    lastFrameRef.current = now;
    rafRef.current = requestAnimationFrame(measure);

    // Skip implausibly large gaps entirely (tab was hidden/backgrounded) so
    // they skew neither the dropped-frame count nor the average frame time.
    if (delta > MAX_PLAUSIBLE_FRAME_MS) return;

    const times = frameTimesRef.current;
    times.push(delta);
    droppedRef.current += droppedFramesForDelta(delta);

    // Publish a rolling window every 30 frames, then reset the counters so a
    // brief hiccup ages out instead of sticking on screen forever.
    if (times.length >= 30) {
      const avg = times.reduce((a, b) => a + b, 0) / times.length;
      setMetrics({
        fps: Math.round(1000 / avg),
        frameTime: Math.round(avg * 100) / 100,
        droppedFrames: droppedRef.current,
      });
      frameTimesRef.current = [];
      droppedRef.current = 0;
    }
  }, []);

  useEffect(() => {
    rafRef.current = requestAnimationFrame(measure);
    return () => cancelAnimationFrame(rafRef.current);
  }, [measure]);

  return metrics;
}
