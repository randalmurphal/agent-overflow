import {
  recordBusy,
  recordBusyDrop,
} from './perfStats';
import type { MeterName, PerfRun } from './perf_types';

interface MemoryPerformance extends Performance {
  memory?: { usedJSHeapSize?: number };
}

export function heapBytes(): number {
  const memory = (performance as MemoryPerformance).memory;
  return typeof memory?.usedJSHeapSize === 'number' ? memory.usedJSHeapSize : 0;
}

function supportedEntryTypes(): readonly string[] {
  if (typeof PerformanceObserver === 'undefined') return [];
  return PerformanceObserver.supportedEntryTypes ?? [];
}

export function observe(
  state: PerfRun,
  meter: MeterName,
  type: string,
  handle: (entries: PerformanceEntryList) => void,
  init: PerformanceObserverInit = {},
): void {
  if (!state.meters.has(meter)) return;
  if (!supportedEntryTypes().includes(type)) {
    state.unavailable.add(meter);
    return;
  }
  try {
    const observer = new PerformanceObserver((list) => handle(list.getEntries()));
    observer.observe({ type, buffered: false, ...init });
    state.observers.push(observer);
  } catch {
    // A type the browser lists but refuses to observe (some entry types
    // need a secure context or a flag). Absent, not fatal.
    state.unavailable.add(meter);
  }
}

export interface LayoutShiftEntry extends PerformanceEntry {
  value?: number;
  hadRecentInput?: boolean;
}

/** Stops the rAF loop and disconnects the observers. Leaves `run` alone. */
export function disarmMeters(state: PerfRun): void {
  if (state.watchdog !== null && typeof clearTimeout === 'function') {
    clearTimeout(state.watchdog);
  }
  state.watchdog = null;
  if (state.rafId !== null && typeof cancelAnimationFrame === 'function') {
    cancelAnimationFrame(state.rafId);
  }
  state.rafId = null;
  if (state.busyChannel !== null) {
    const channel = state.busyChannel;
    state.busyChannel = null;
    try {
      channel.port1.onmessage = null;
      channel.port1.close();
      channel.port2.close();
    } catch {
      // A closed port is the goal; a throw here changes nothing.
    }
  }
  state.busyPending = false;
  for (const observer of state.observers) {
    try {
      observer.disconnect();
    } catch {
      // A disconnected observer is the goal; a throw here changes nothing.
    }
  }
  state.observers = [];
}

/**
 * Builds the busy meter's probe channel, once, for the life of the run.
 * False when this engine has none — feature detection, never engine
 * sniffing, and the run reports the meter absent rather than failing.
 *
 * The measurement is "time until the main thread could service a cheap
 * task", so the task has to BE cheap and has to be a task: a MessageChannel
 * post is the standard zero-work macrotask, and a macrotask is what the
 * engine runs only after this tick's remaining callbacks, style, layout and
 * paint are done. A microtask would resolve inside the callback and measure
 * nothing.
 */
export function armBusyProbe(state: PerfRun, isCurrent: (state: PerfRun) => boolean): boolean {
  if (typeof MessageChannel !== 'function') return false;
  try {
    const channel = new MessageChannel();
    channel.port1.onmessage = (event: MessageEvent): void => {
      if (!isCurrent(state) || !state.busyPending) return;
      // A probe whose tick was superseded still arrives. Its sequence is
      // stale, and the interval it would report spans two ticks.
      if (event.data !== state.busySeq) return;
      state.busyPending = false;
      const busyMs = performance.now() - state.busyStartedAt;
      // The tick's own ENTRY timestamp rides along, not the reply's: the
      // worst-tick list names when the expensive work STARTED, which is
      // where a trace has to be opened.
      recordBusy(state.busy, busyMs, state.busyStartedAt);
      if (!Number.isFinite(busyMs) || busyMs < 0) return;
      if (busyMs > state.windowBusyMaxMs) state.windowBusyMaxMs = busyMs;
      state.windowBusySumMs += busyMs;
    };
    state.busyChannel = channel;
    return true;
  } catch {
    // A channel the engine lists but refuses to build. Absent, not fatal.
    return false;
  }
}

/**
 * Posts one tick's probe. ONE measurement is in flight at a time: when the
 * previous tick's probe has not answered, the frame it would be attributed
 * to is already over, so it is DROPPED (and counted as such) rather than
 * charged to whichever tick happens to be running when it lands.
 */
export function startBusyProbe(state: PerfRun, entryMs: number): void {
  const channel = state.busyChannel;
  if (channel === null) return;
  if (state.busyPending) recordBusyDrop(state.busy);
  state.busySeq += 1;
  state.busyPending = true;
  state.busyStartedAt = entryMs;
  try {
    channel.port2.postMessage(state.busySeq);
  } catch {
    // Nothing is in flight if the post never happened; leaving `pending`
    // set would drop the NEXT tick too, for a failure that was this one's.
    state.busyPending = false;
  }
}
