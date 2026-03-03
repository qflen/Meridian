import { describe, it, expect } from 'vitest';
import {
  droppedFramesForDelta,
  droppedFramesInWindow,
  TARGET_FRAME_MS,
} from './frameMetrics';

describe('droppedFramesForDelta', () => {
  it('counts nothing for an on-target frame', () => {
    expect(droppedFramesForDelta(TARGET_FRAME_MS)).toBe(0);
    expect(droppedFramesForDelta(16)).toBe(0);
    expect(droppedFramesForDelta(30)).toBe(0); // within 2x target
  });

  it('counts a single missed frame', () => {
    expect(droppedFramesForDelta(34)).toBe(1);
    expect(droppedFramesForDelta(40)).toBe(1);
  });

  it('counts longer jank proportionally', () => {
    expect(droppedFramesForDelta(60)).toBe(2);
    expect(droppedFramesForDelta(90)).toBe(4);
  });

  it('ignores implausibly large deltas (hidden / throttled tab)', () => {
    expect(droppedFramesForDelta(1000)).toBe(0);
    expect(droppedFramesForDelta(60000)).toBe(0);
  });

  it('ignores non-positive or non-finite deltas', () => {
    expect(droppedFramesForDelta(0)).toBe(0);
    expect(droppedFramesForDelta(-5)).toBe(0);
    expect(droppedFramesForDelta(NaN)).toBe(0);
  });
});

describe('droppedFramesInWindow', () => {
  it('sums drops across a window, ignoring hidden-tab gaps', () => {
    // 16,16 clean (0); 34 -> 1; 60 -> 2; 5000 hidden -> 0
    expect(droppedFramesInWindow([16, 16, 34, 60, 5000])).toBe(3);
  });

  it('is 0 for a steady 60fps window', () => {
    expect(droppedFramesInWindow(new Array(30).fill(TARGET_FRAME_MS))).toBe(0);
  });
});
