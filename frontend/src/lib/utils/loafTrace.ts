// Long-animation-frame (LoAF) capture for the ui-trace pipeline.
//
// Closes the two coverage gaps the existing frame telemetry has:
//   - The spring's chase telemetry (spring.ts beginChaseTelemetry) only
//     measures while a chase is running — a slow frame between chases,
//     or on a pane that isn't gliding, is invisible to it.
//   - Its 'longtask' observer counts long TASKS, not long FRAMES: a
//     frame whose JS was fine but whose style/layout/paint phases blew
//     the budget produces no longtask entry, and longtask carries no
//     attribution.
// LoAF measures the whole frame (script + rendering phases) with script
// attribution, continuously for the session. That makes a bug-report
// capture self-discriminating for the "pixels froze then jumped" class:
// a visible jump with clean `frame.loaf` records AND clean chase cadence
// is renderer-exonerating evidence — the frames died after commit, in
// the compositor/presentation path (WebView2/DWM), which no renderer-
// side instrument can see.
//
// The 50ms reporting threshold is fixed by spec — LoAF cannot be tuned
// lower, so sub-50ms frame drops stay invisible here (the chase
// telemetry's gap buckets cover those, during chases). That threshold
// is also why this is light-tier (UI_TRACE=1, not oracle-tier): the
// browser only delivers entries for frames that exceeded it, so there
// is no per-frame observer cost in the steady state.

import { isUiRenderTraceEnabled, recordUiTrace } from './uiRenderTrace';

// Attribution entries per frame record. LoAF reports every script over
// 5ms; a pathological frame can carry dozens. The record keeps the top
// few by duration plus the total count, so truncation is visible.
const MAX_SCRIPTS_PER_RECORD = 3;

const LOAF_ENTRY_TYPE = 'long-animation-frame';

// ===== JS-heap sampling (GC discrimination) =====
// A script-less LoAF (no scripts, no blocking, renderStart delayed the
// whole stall) is either a major GC pause or something outside the
// renderer (compositor, OS contention). The two need different fixes,
// and telling them apart used to need a live devtools session at the
// moment of the stall. `performance.memory.usedJSHeapSize` is the
// always-on discriminator: a heap DROP of tens of MB across the stall
// window is a GC verdict; flat heap exonerates the renderer. The
// sampler is a coarse interval (no rAF cost), the ring holds enough
// history to cover LoAF delivery delay, and everything degrades to
// absent fields where `performance.memory` doesn't exist.
const HEAP_SAMPLE_INTERVAL_MS = 2000;
const HEAP_RING_SIZE = 8;

interface HeapSample {
  t: number; // performance.now() timebase, comparable to entry.startTime
  usedMb: number;
}

interface ChromeMemory {
  usedJSHeapSize: number;
}

function readHeapMb(): number | null {
  const memory = (performance as { memory?: ChromeMemory }).memory;
  if (!memory || typeof memory.usedJSHeapSize !== 'number') return null;
  return Math.round(memory.usedJSHeapSize / 1048576);
}

const heapRing: HeapSample[] = [];

function sampleHeap(): void {
  const usedMb = readHeapMb();
  if (usedMb === null) return;
  heapRing.push({ t: performance.now(), usedMb });
  if (heapRing.length > HEAP_RING_SIZE) heapRing.shift();
}

// Latest sample taken at-or-before the frame started — the heap the
// stall entered with. LoAF entries arrive batched and late, so "the
// previous callback's value" would straddle the wrong window.
function heapBefore(startTime: number): number | null {
  for (let i = heapRing.length - 1; i >= 0; i--) {
    const sample = heapRing[i];
    if (sample.t <= startTime) return sample.usedMb;
  }
  return null;
}

// lib.dom does not ship LoAF typings yet; these mirror the spec fields
// this module reads (https://w3c.github.io/long-animation-frames/).
interface PerformanceScriptTiming extends PerformanceEntry {
  invoker: string;
  invokerType: string;
  sourceURL: string;
  sourceFunctionName: string;
  forcedStyleAndLayoutDuration: number;
}

interface PerformanceLongAnimationFrameTiming extends PerformanceEntry {
  renderStart: number;
  styleAndLayoutStart: number;
  blockingDuration: number;
  firstUIEventTimestamp: number;
  scripts: PerformanceScriptTiming[];
}

function round(value: number): number {
  return Math.round(value * 10) / 10;
}

// Vite dev URLs are long and identical up to the path; keep the tail
// that identifies the module so records stay under the per-line cap.
function shortSourceURL(url: string): string {
  const withoutQuery = url.split('?')[0];
  const parts = withoutQuery.split('/');
  return parts.slice(-2).join('/');
}

function buildLoafRecord(entry: PerformanceLongAnimationFrameTiming): unknown {
  const heapNowMb = readHeapMb();
  const heapBeforeMb = heapBefore(entry.startTime);
  const scripts = [...entry.scripts]
    .sort((a, b) => b.duration - a.duration)
    .slice(0, MAX_SCRIPTS_PER_RECORD)
    .map((script) => ({
      invoker: script.invoker,
      invokerType: script.invokerType,
      source: shortSourceURL(script.sourceURL),
      fn: script.sourceFunctionName || undefined,
      durationMs: round(script.duration),
      forcedStyleLayoutMs: round(script.forcedStyleAndLayoutDuration),
    }));
  return {
    startTime: round(entry.startTime),
    durationMs: round(entry.duration),
    // Time the main thread was unavailable to input (the responsiveness
    // component of the frame — 0 for a frame that was long but yielded).
    blockingMs: round(entry.blockingDuration),
    // Rendering-phase timestamps (0 when the frame never reached the
    // phase). renderStart→end is requestAnimationFrame callbacks +
    // style/layout; styleAndLayoutStart→end is style/layout alone.
    renderStart: round(entry.renderStart),
    styleAndLayoutStart: round(entry.styleAndLayoutStart),
    firstUIEventTimestamp: round(entry.firstUIEventTimestamp),
    // GC discrimination for script-less stalls: heap at record time vs
    // the last interval sample before the frame started. A large drop
    // says the stall was a major GC; flat says look outside the
    // renderer. Absent where performance.memory doesn't exist.
    heapNowMb: heapNowMb ?? undefined,
    heapBeforeMb: heapBeforeMb ?? undefined,
    scriptCount: entry.scripts.length,
    scripts,
  };
}

/**
 * Register the standing LoAF observer. Called once at app init (beside
 * `installUiRenderTraceApi`); returns a disconnect cleanup. In builds
 * without UI_TRACE the runtime gate is compile-time false and this is a
 * no-op. The one `frame.loaf.install` record makes captures
 * self-evident: no `frame.loaf` records in a trace that carries the
 * install record means no frame exceeded 50ms — absence is evidence,
 * not a missing instrument.
 */
export function installLoafTrace(): () => void {
  if (!isUiRenderTraceEnabled()) return () => {};
  const supported =
    typeof PerformanceObserver !== 'undefined'
    && (PerformanceObserver.supportedEntryTypes ?? []).includes(LOAF_ENTRY_TYPE);
  if (!supported) {
    recordUiTrace('frame.loaf.install', { supported: false });
    return () => {};
  }
  let observer: PerformanceObserver | null = null;
  try {
    observer = new PerformanceObserver((list) => {
      for (const entry of list.getEntries()) {
        recordUiTrace(
          'frame.loaf',
          buildLoafRecord(entry as PerformanceLongAnimationFrameTiming),
        );
      }
    });
    // buffered: frames from before install (startup hydrate/restore)
    // are exactly the ones nothing else measures.
    observer.observe({ type: LOAF_ENTRY_TYPE, buffered: true });
  } catch (error) {
    // Engines can advertise an entry type and still throw on observe
    // (same degradation path as the spring's longtask observer). The
    // install record carries the failure so a capture without loaf
    // records is never misread as a clean session.
    recordUiTrace('frame.loaf.install', {
      supported: false,
      observeError: error instanceof Error ? error.message : String(error),
    });
    return () => {};
  }
  recordUiTrace('frame.loaf.install', { supported: true });
  // Heap sampler armed only where the heap is readable at all — a
  // permanently-null reading buys nothing for its interval.
  let heapTimer: ReturnType<typeof setInterval> | null = null;
  if (readHeapMb() !== null) {
    sampleHeap();
    heapTimer = setInterval(sampleHeap, HEAP_SAMPLE_INTERVAL_MS);
  }
  return () => {
    observer?.disconnect();
    observer = null;
    if (heapTimer !== null) clearInterval(heapTimer);
    heapTimer = null;
    heapRing.length = 0;
  };
}
