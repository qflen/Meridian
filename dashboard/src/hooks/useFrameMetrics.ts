import { useRef, useCallback, useEffect, useState } from 'react';

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

    const times = frameTimesRef.current;
    times.push(delta);

    if (delta > 33.33) {
      droppedRef.current += Math.floor(delta / 16.67) - 1;
    }

    // Update every 30 frames
    if (times.length >= 30) {
      const avg = times.reduce((a, b) => a + b, 0) / times.length;
      setMetrics({
        fps: Math.round(1000 / avg),
        frameTime: Math.round(avg * 100) / 100,
        droppedFrames: droppedRef.current,
      });
      frameTimesRef.current = [];
    }

    rafRef.current = requestAnimationFrame(measure);
  }, []);

  useEffect(() => {
    rafRef.current = requestAnimationFrame(measure);
    return () => cancelAnimationFrame(rafRef.current);
  }, [measure]);

  return metrics;
}
