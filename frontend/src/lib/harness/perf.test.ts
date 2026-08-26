// The perf run's own semantics: what one sample means, what a meter set
// buys, and the two ways a run can end. The dispatch-level half (error
// envelopes, the no-reply sentinel) lives in bridge.test.ts, and the
// engine-level half (real rAF cadence, real observers) in
// e2e/tests/harness-bridge.spec.ts — happy-dom has neither.
//
// The frame meter is driven through a hand-rolled rAF queue rather than a
// timer: a frame TIME is the whole subject here, so the test has to name
// the timestamps rather than hope for them.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  PERF_WATCHDOG_MS,
  collectPerfSample,
  perfRunActive,
  perfRunAddressed,
  perfRunId,
  perfSelfDisarmMessage,
  startPerfRun,
  stopPerfRun,
  unknownPerfMeters,
} from './perf';

const realRaf = globalThis.requestAnimationFrame;
const realCancelRaf = globalThis.cancelAnimationFrame;
let pendingFrames: FrameRequestCallback[] = [];

beforeEach(() => {
  pendingFrames = [];
  globalThis.requestAnimationFrame = ((cb: FrameRequestCallback) =>
    pendingFrames.push(cb)) as typeof requestAnimationFrame;
  globalThis.cancelAnimationFrame = (() => {
    pendingFrames = [];
  }) as typeof cancelAnimationFrame;
});

afterEach(() => {
  if (perfRunActive()) stopPerfRun();
  globalThis.requestAnimationFrame = realRaf;
  globalThis.cancelAnimationFrame = realCancelRaf;
  document.body.innerHTML = '';
});

/**
 * Fake ONLY the timer pair the watchdog uses. Vitest's default set also
 * replaces requestAnimationFrame, which would take the frame queue above
 * away from the test that has to see the loop stop.
 */
function useTimerFakes(): void {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
}

/** Runs the queued rAF callback once per timestamp, as a browser would. */
function paint(...timestamps: number[]): void {
  for (const timestamp of timestamps) {
    const due = pendingFrames;
    pendingFrames = [];
    for (const cb of due) cb(timestamp);
  }
}

describe('frame window', () => {
  // maxFrameMs is documented (cmd/ao-harness/cmd_perf.go) as the per-sample
  // worst frame, and a live watcher reads it as one. Deriving it from the
  // run-wide histogram max made every sample after a stall report that
  // stall forever.
  it('reports the worst frame of THIS window, not of the run', () => {
    startPerfRun({ meters: ['frames'] });
    paint(0, 16, 416);
    const stalled = collectPerfSample();
    expect(stalled.frames).toBe(2);
    expect(stalled.maxFrameMs).toBe(400);

    paint(432, 448);
    const calm = collectPerfSample();
    expect(calm.frames).toBe(2);
    expect(calm.maxFrameMs).toBe(16);

    // The run-wide worst is still the summary's answer — the per-window
    // reset must not cost the distribution.
    const summary = stopPerfRun();
    expect(summary?.frames.maxMs).toBe(400);
    expect(summary?.frames.frames).toBe(4);
  });

  it('counts frames and long frames per window too', () => {
    startPerfRun({ meters: ['frames'], longFrameMs: 100 });
    paint(0, 16, 32, 400);
    const first = collectPerfSample();
    expect(first).toMatchObject({ frames: 3, longFrames: 1 });
    paint(416);
    expect(collectPerfSample()).toMatchObject({ frames: 1, longFrames: 0 });
  });

  // rAF timestamps go backwards across a suspend/restore in more than one
  // engine, and perfStats drops such a delta. The window max has to agree,
  // or the sample reports a frame the histogram refused to record.
  it('ignores a backwards frame delta', () => {
    startPerfRun({ meters: ['frames'] });
    paint(1000, 900);
    expect(collectPerfSample()).toMatchObject({ frames: 0, maxFrameMs: 0 });
  });
});

describe('meter selection', () => {
  it('names the meters it does not know', () => {
    expect(unknownPerfMeters(['frames', 'dom'])).toEqual([]);
    expect(unknownPerfMeters(['fps', 'frames', 'gpu'])).toEqual(['fps', 'gpu']);
  });

  // countPaneRows is a document-wide querySelectorAll plus one per pane.
  // A run that excluded `dom` asked for that walk not to happen: the whole
  // point of narrowing the meter set is to stop paying for what is not
  // being measured.
  it('skips the pane walk entirely when the dom meter is out', () => {
    document.body.innerHTML =
      '<section data-pane-id="pane-a"><div data-row-index="0"></div></section>';

    startPerfRun({ meters: ['dom'] });
    const withDom = collectPerfSample();
    expect(withDom.panes).toEqual([{ paneId: 'pane-a', rows: 1 }]);
    expect(withDom.domNodes).toBeGreaterThan(0);
    stopPerfRun();

    startPerfRun({ meters: ['frames'] });
    const withoutDom = collectPerfSample();
    expect(withoutDom.panes).toEqual([]);
    expect(withoutDom.domNodes).toBe(0);
  });
});

describe('run addressing', () => {
  // Several pages can be attached to one backend and every one of them
  // sees every ui-query. Only the page that armed the run may answer for
  // it; the rest must stay silent rather than race a refusal in.
  it('claims only the run it armed, and every unstamped query', () => {
    expect(perfRunAddressed('')).toBe(true);
    expect(perfRunAddressed('perf-9')).toBe(false);

    startPerfRun({ meters: ['dom'], runId: 'perf-9' });
    expect(perfRunId()).toBe('perf-9');
    expect(perfRunAddressed('perf-9')).toBe(true);
    expect(perfRunAddressed('perf-10')).toBe(false);
    // A backend that does not stamp ids gets the pre-runId behaviour.
    expect(perfRunAddressed('')).toBe(true);
  });

  // A backend that stamps its collects but not its arms would otherwise
  // silence the one page that CAN answer — the page has no id to compare
  // and must not read that as "not mine".
  it('answers a stamped query when it was armed without an id', () => {
    startPerfRun({ meters: ['dom'] });
    expect(perfRunId()).toBe('');
    expect(perfRunAddressed('perf-1')).toBe(true);
    expect(perfRunAddressed('')).toBe(true);
  });

  it('keeps answering for a run it self-disarmed, so the caller learns why', () => {
    useTimerFakes();
    try {
      startPerfRun({ meters: ['dom'], runId: 'perf-3' });
      vi.advanceTimersByTime(PERF_WATCHDOG_MS);
      expect(perfRunActive()).toBe(false);
      expect(perfRunAddressed('perf-3')).toBe(true);
      expect(perfRunAddressed('perf-4')).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('watchdog', () => {
  it('disarms a run whose caller stopped collecting', () => {
    useTimerFakes();
    try {
      startPerfRun({ meters: ['dom'], runId: 'perf-1' });
      vi.advanceTimersByTime(PERF_WATCHDOG_MS - 1);
      expect(perfRunActive()).toBe(true);
      expect(perfSelfDisarmMessage()).toBeNull();

      vi.advanceTimersByTime(1);
      expect(perfRunActive()).toBe(false);
      expect(perfSelfDisarmMessage()).toBe(
        `perf run self-disarmed after ${PERF_WATCHDOG_MS}ms without a collect`,
      );
    } finally {
      vi.useRealTimers();
    }
  });

  it('is pushed out by every collect, so a live run never trips it', () => {
    useTimerFakes();
    try {
      startPerfRun({ meters: ['dom'] });
      for (let tick = 0; tick < 4; tick += 1) {
        vi.advanceTimersByTime(PERF_WATCHDOG_MS - 1);
        collectPerfSample();
      }
      vi.advanceTimersByTime(PERF_WATCHDOG_MS - 1);
      expect(perfRunActive()).toBe(true);
      expect(perfSelfDisarmMessage()).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it('stops the frame loop when it fires, not just the bookkeeping', () => {
    useTimerFakes();
    try {
      startPerfRun({ meters: ['frames'] });
      const armedFrames = pendingFrames.length;
      expect(armedFrames).toBeGreaterThan(0);
      vi.advanceTimersByTime(PERF_WATCHDOG_MS);
      // cancelAnimationFrame drains the queue in this fixture exactly as a
      // browser stops calling back: nothing is left to re-queue itself.
      expect(pendingFrames).toHaveLength(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it('clears the self-disarm flag on the next arm', () => {
    useTimerFakes();
    try {
      startPerfRun({ meters: ['dom'], runId: 'perf-1' });
      vi.advanceTimersByTime(PERF_WATCHDOG_MS);
      expect(perfSelfDisarmMessage()).not.toBeNull();
      startPerfRun({ meters: ['dom'], runId: 'perf-2' });
      expect(perfSelfDisarmMessage()).toBeNull();
      expect(perfRunActive()).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it('does not fire after a normal stop', () => {
    useTimerFakes();
    try {
      startPerfRun({ meters: ['dom'] });
      expect(stopPerfRun()).not.toBeNull();
      vi.advanceTimersByTime(PERF_WATCHDOG_MS * 2);
      expect(perfSelfDisarmMessage()).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });
});
