// Records the frames and timers a module schedules itself: calls to
// requestAnimationFrame, setTimeout and setInterval whose direct caller is
// in a file whose URL contains `file`, except from the functions named in
// `except` (a scan's own yield, say). Browser project only (it reads the
// V8 stack). Restore it in the test's finally.

export interface SchedulingSpy {
  /** The calls since the spy started or was last cleared, by kind and caller. */
  readonly calls: string[];
  clear(): void;
  restore(): void;
}

export function spyScheduling(file: string, except: readonly string[] = []): SchedulingSpy {
  const calls: string[] = [];
  const frame = window.requestAnimationFrame;
  const timeout = window.setTimeout;
  const interval = window.setInterval;
  const note = (kind: string) => {
    // The frames outside this file, from the wrapper's caller on.
    const frames = (new Error().stack ?? '').split('\n').slice(1).filter((line) => !line.includes('schedulingSpy')).map((line) => line.trim());
    const at = frames[0] ?? '';
    if (!at.includes(file)) return;
    // The caller is an arrow inside a named function of the file (a
    // promise executor): the name is on a frame within the file. V8
    // prefixes it with the receiver's type when the function is called as
    // a property ("Object.pause"), so the name is the last segment.
    const named = frames.slice(0, 3).filter((line) => line.includes(file)).map((line) => /^at (?:async )?(?:\S+\.)?([^\s.]+) \(/.exec(line)?.[1] ?? '');
    if (named.some((name) => except.includes(name))) return;
    calls.push(`${kind} ${at}`);
  };
  window.requestAnimationFrame = function (callback: FrameRequestCallback): number {
    note('frame');
    return frame.call(window, callback);
  };
  window.setTimeout = function (...args: Parameters<typeof setTimeout>) {
    note('timeout');
    return timeout.apply(window, args);
  } as typeof setTimeout;
  window.setInterval = function (...args: Parameters<typeof setInterval>) {
    note('interval');
    return interval.apply(window, args);
  } as typeof setInterval;
  return {
    calls,
    clear() {
      calls.length = 0;
    },
    restore() {
      window.requestAnimationFrame = frame;
      window.setTimeout = timeout;
      window.setInterval = interval;
    },
  };
}
