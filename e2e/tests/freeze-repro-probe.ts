// Renderer-liveness monitor and CDP evidence capture for
// freeze-repro.manual.spec.ts.
//
// The thing being detected is a wedged renderer MAIN THREAD, which is exactly
// the condition that makes ordinary Playwright assertions useless: every
// locator, every evaluate, every screenshot queues behind the same blocked
// thread. So liveness is measured OUT OF BAND — probes are fired on a Node
// timer and never awaited in sequence, and a separate watchdog decides the
// renderer is wedged when no probe has come back for long enough.
//
// Both capture channels are armed BEFORE the replay starts, because both of
// them need something from the renderer that a wedge can withhold:
//
//   - `Profiler.stop` is answered on the main thread. A profiler started early
//     has already sampled the spinning stack, and a wedge that self-heals (the
//     svelte flush caps and the fork watchdog are both designed to) hands the
//     profile over the moment it does — but a wedge that never clears never
//     answers, so its deadline is a bound, not a wait.
//   - `Debugger.pause` rides the V8 interrupt path and CAN break into a
//     spinning JS loop — but only once the Debugger domain is live, and
//     `Debugger.enable` is itself main-thread-answered. Enabling it on demand
//     was measured hanging with everything else (observed 2026-08-10: a wedge
//     that outlived a 10-minute Profiler.stop and a 5s Debugger.enable),
//     so the domain is enabled up front instead.
//
// Enabling the Debugger domain for the whole replay costs some V8 optimization
// and is a deliberate trade: on a permanent wedge it is the only channel that
// yields a stack at all, and a driver that reproduces a freeze without naming
// a frame is not worth running.

import { mkdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import type { BrowserContext, CDPSession, Page } from '@playwright/test';

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

/** Reject rather than hang forever when the renderer never answers. */
function withDeadline<T>(promise: Promise<T>, ms: number, what: string): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`${what} did not answer within ${ms}ms`)), ms);
    promise.then(
      (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      (err) => {
        clearTimeout(timer);
        reject(err instanceof Error ? err : new Error(String(err)));
      },
    );
  });
}

export interface WedgeInfo {
  /** Milliseconds since the last probe came back, at detection time. */
  gapMs: number;
  detectedAt: number;
}

export interface RendererMonitor {
  /** Longest observed gap between successive probe resolutions, in ms. */
  readonly longestGapMs: number;
  /** Resolves once no probe has resolved for longer than the wedge threshold. */
  readonly wedged: Promise<WedgeInfo>;
  readonly isWedged: boolean;
  /** Number of probes that have come back. */
  readonly probesResolved: number;
  /**
   * Probes that came back as a REJECTION (closed page, navigated frame,
   * crashed renderer). A rejection is not evidence of a wedge, but it is not
   * nothing either: `lastResolvedAt` stops advancing, so a run whose page
   * died reports the same "no probe came back" the watchdog reports for a
   * real freeze. The verdict has to be able to tell them apart, which means
   * the count has to leave this module.
   */
  readonly probesRejected: number;
  /** The last rejection's message, or null when none has happened. */
  readonly lastRejection: string | null;
  /**
   * ms from wedge detection to the first probe that answered afterwards, or
   * null while the renderer is still wedged. This is how long the freeze
   * actually lasted — the detection threshold only says when we noticed.
   */
  readonly recoveredAfterMs: number | null;
  stop(): void;
}

export interface MonitorOptions {
  probeIntervalMs?: number;
  watchdogIntervalMs?: number;
  wedgeMs?: number;
}

/**
 * Fire `performance.now()` at the renderer on a fixed Node timer and watch how
 * long answers take to come back.
 *
 * Probes are never awaited in sequence: one stalled evaluate must not stop the
 * next probe from being issued, or a wedge would look identical to a slow
 * loop. Everything is decided from `lastResolvedAt` on a separate watchdog.
 */
export function startRendererMonitor(page: Page, options: MonitorOptions = {}): RendererMonitor {
  const probeIntervalMs = options.probeIntervalMs ?? 500;
  const watchdogIntervalMs = options.watchdogIntervalMs ?? 250;
  const wedgeMs = options.wedgeMs ?? 12_000;

  let lastResolvedAt = Date.now();
  let longestGapMs = 0;
  let probesResolved = 0;
  let probesRejected = 0;
  let lastRejection: string | null = null;
  let stopped = false;
  let wedgeSettled = false;
  let wedgeDetectedAt = 0;
  let recoveredAfterMs: number | null = null;
  let resolveWedge!: (info: WedgeInfo) => void;
  const wedged = new Promise<WedgeInfo>((resolve) => {
    resolveWedge = resolve;
  });

  const probeTimer = setInterval(() => {
    if (stopped) return;
    page.evaluate(() => performance.now()).then(
      () => {
        if (stopped) return;
        probesResolved += 1;
        if (wedgeSettled && recoveredAfterMs === null) {
          recoveredAfterMs = Date.now() - wedgeDetectedAt;
        }
        // Monotonic: an out-of-order resolve must never rewind liveness.
        lastResolvedAt = Math.max(lastResolvedAt, Date.now());
      },
      (err: unknown) => {
        if (stopped) return;
        // A closed page / navigated frame rejects. Not evidence of a wedge —
        // the watchdog keeps measuring from the last real answer — but it is
        // recorded, because silence FROM a dead page and silence from a
        // wedged one are indistinguishable in `longestGapMs` alone, and a
        // discarded rejection made the difference unrecoverable.
        probesRejected += 1;
        lastRejection = err instanceof Error ? err.message : String(err);
      },
    );
  }, probeIntervalMs);

  const watchdogTimer = setInterval(() => {
    if (stopped) return;
    const gap = Date.now() - lastResolvedAt;
    if (gap > longestGapMs) longestGapMs = gap;
    if (!wedgeSettled && gap > wedgeMs) {
      wedgeSettled = true;
      wedgeDetectedAt = Date.now();
      resolveWedge({ gapMs: gap, detectedAt: wedgeDetectedAt });
    }
  }, watchdogIntervalMs);

  return {
    get longestGapMs() {
      return longestGapMs;
    },
    get isWedged() {
      return wedgeSettled;
    },
    get probesResolved() {
      return probesResolved;
    },
    get probesRejected() {
      return probesRejected;
    },
    get lastRejection() {
      return lastRejection;
    },
    get recoveredAfterMs() {
      return recoveredAfterMs;
    },
    wedged,
    stop() {
      stopped = true;
      clearInterval(probeTimer);
      clearInterval(watchdogTimer);
    },
  };
}

/** Bounded capture of everything the page said, from before the first turn. */
export function captureConsole(page: Page, cap = 5000): string[] {
  const lines: string[] = [];
  const push = (line: string) => {
    lines.push(`${new Date().toISOString()} ${line}`);
    if (lines.length > cap) lines.shift();
  };
  page.on('console', (msg) => push(`[${msg.type()}] ${msg.text()}`));
  page.on('pageerror', (err) => push(`[pageerror] ${err.stack ?? err.message}`));
  page.on('crash', () => push('[crash] the page crashed'));
  return lines;
}

export interface CaptureSessions {
  /** Sampling profiler, running from before the first live turn. */
  profiler: CDPSession;
  /** Debugger domain, enabled up front so `pause` can interrupt a wedge. */
  debug: CDPSession;
  /** scriptId → url, accumulated from Debugger.scriptParsed. */
  scripts: Map<string, string>;
}

/**
 * Arm both capture channels while the renderer is still healthy.
 *
 * 1000µs sampling is cheap enough to leave running for the whole replay and
 * fine enough that a multi-second wedge lands thousands of samples in the
 * spinning frame. The two domains get separate CDP sessions so a hung
 * `Profiler.stop` can never sit in front of a `Debugger.pause`.
 */
export async function startCaptureSessions(
  context: BrowserContext,
  page: Page,
  samplingIntervalUs = 1000,
): Promise<CaptureSessions> {
  const profiler = await context.newCDPSession(page);
  await profiler.send('Profiler.enable');
  await profiler.send('Profiler.setSamplingInterval', { interval: samplingIntervalUs });
  await profiler.send('Profiler.start');

  const debug = await context.newCDPSession(page);
  // `Debugger.paused` call frames carry a scriptId, not always a url. Collect
  // the mapping as scripts load so a captured frame names its bundle.
  const scripts = new Map<string, string>();
  debug.on('Debugger.scriptParsed', (event: unknown) => {
    const parsed = event as { scriptId?: string; url?: string };
    if (parsed.scriptId) scripts.set(parsed.scriptId, parsed.url ?? '');
  });
  await debug.send('Debugger.enable');
  return { profiler, debug, scripts };
}

interface CallFrame {
  functionName: string;
  url: string;
  scriptId: string;
  lineNumber: number;
  columnNumber?: number;
  /**
   * The bundle text around the frame's position. Production bundles are
   * minified, so `xY @ index-abc.js:50` alone names nothing — the surrounding
   * source is what makes a captured stack actionable.
   */
  source?: string;
}

/**
 * A bounded slice of a script around one position. Fetching the script is only
 * possible while the inspector is servicing us (i.e. while paused), so this
 * runs inside the pause window and caches per script.
 */
async function sourceAround(
  session: CDPSession,
  cache: Map<string, string[]>,
  scriptId: string,
  lineNumber: number,
  columnNumber: number,
  radius = 160,
): Promise<string | undefined> {
  try {
    let lines = cache.get(scriptId);
    if (!lines) {
      const result = (await withDeadline(
        session.send('Debugger.getScriptSource', { scriptId }),
        10_000,
        'Debugger.getScriptSource',
      )) as { scriptSource: string };
      lines = result.scriptSource.split('\n');
      cache.set(scriptId, lines);
    }
    const line = lines[lineNumber];
    if (line === undefined) return undefined;
    const from = Math.max(0, columnNumber - radius);
    return `${from > 0 ? '…' : ''}${line.slice(from, columnNumber + radius)}${
      columnNumber + radius < line.length ? '…' : ''
    }`;
  } catch {
    return undefined;
  }
}

interface StackSample {
  sample: number;
  at: number;
  callFrames?: CallFrame[];
  error?: string;
  /**
   * Why this sample's `Debugger.resume` failed, when it did.
   *
   * Load-bearing rather than diagnostic garnish: every LATER sample pauses a
   * target that is still paused from this one, so their stacks describe where
   * this sample stopped the renderer and not where it spins. Swallowing the
   * failure left those samples looking like independent evidence.
   */
  resumeError?: string;
}

/**
 * Pause the renderer through the V8 interrupt path and read the paused stack,
 * a few times, resuming between samples. Runs on its own CDP session so it is
 * never queued behind the profiler's main-thread-answered commands.
 */
interface WirePausedFrame {
  functionName: string;
  url?: string;
  location: { scriptId: string; lineNumber: number; columnNumber?: number };
}

/** Frames deep enough to be caller context rather than the spin itself. */
const SOURCE_SNIPPET_FRAMES = 8;

async function collectPausedStacks(
  session: CDPSession,
  scripts: Map<string, string>,
  samples: number,
  spacingMs: number,
  deadlineMs: number,
): Promise<StackSample[]> {
  const out: StackSample[] = [];
  const sourceCache = new Map<string, string[]>();
  for (let i = 0; i < samples; i += 1) {
    try {
      // Register the listener before the command: a wedged thread can deliver
      // `paused` the instant the interrupt lands.
      const paused = new Promise<{ callFrames: WirePausedFrame[] }>((resolve, reject) => {
        const timer = setTimeout(
          () => reject(new Error(`Debugger.paused not delivered within ${deadlineMs}ms`)),
          deadlineMs,
        );
        session.once('Debugger.paused', (event: unknown) => {
          clearTimeout(timer);
          resolve(event as { callFrames: WirePausedFrame[] });
        });
      });
      await withDeadline(session.send('Debugger.pause'), deadlineMs, 'Debugger.pause');
      const event = await paused;

      const callFrames: CallFrame[] = [];
      for (const [depth, frame] of event.callFrames.entries()) {
        const columnNumber = frame.location.columnNumber ?? 0;
        callFrames.push({
          functionName: frame.functionName || '(anonymous)',
          url: frame.url || scripts.get(frame.location.scriptId) || '',
          scriptId: frame.location.scriptId,
          lineNumber: frame.location.lineNumber,
          columnNumber,
          // Still paused here, so the inspector answers.
          source:
            depth < SOURCE_SNIPPET_FRAMES
              ? await sourceAround(
                  session,
                  sourceCache,
                  frame.location.scriptId,
                  frame.location.lineNumber,
                  columnNumber,
                )
              : undefined,
        });
      }
      const stack: StackSample = { sample: i, at: Date.now(), callFrames };
      out.push(stack);

      try {
        await withDeadline(session.send('Debugger.resume'), deadlineMs, 'Debugger.resume');
      } catch (err) {
        stack.resumeError = err instanceof Error ? err.message : String(err);
      }
    } catch (err) {
      out.push({ sample: i, at: Date.now(), error: String(err) });
    }
    await sleep(spacingMs);
  }
  // The session belongs to the caller (armed up front); leave it attached.
  return out;
}

interface ProfileNode {
  id: number;
  hitCount?: number;
  callFrame: { functionName: string; url: string; lineNumber: number };
}

/** Hottest self-time frames in a .cpuprofile — the profiler's answer to "where". */
function hottestFrames(profile: { nodes?: ProfileNode[] }, limit: number): string[] {
  const nodes = profile.nodes ?? [];
  return [...nodes]
    .sort((a, b) => (b.hitCount ?? 0) - (a.hitCount ?? 0))
    .slice(0, limit)
    .filter((node) => (node.hitCount ?? 0) > 0)
    .map(
      (node) =>
        `${node.hitCount} samples  ${node.callFrame.functionName || '(anonymous)'} ` +
        `@ ${node.callFrame.url}:${node.callFrame.lineNumber + 1}`,
    );
}

export interface EvidenceResult {
  dir: string;
  topFrames: string[];
  pausedStacks: StackSample[];
  profileSaved: boolean;
  profileError?: string;
}

/**
 * Write everything worth having about a wedge into `dir`. Every step is
 * independently guarded: one hanging capture must not cost the others.
 */
export async function captureWedgeEvidence(opts: {
  sessions: CaptureSessions;
  dir: string;
  consoleLines: string[];
  wedge: WedgeInfo;
  monitor: RendererMonitor;
  dwellMs?: number;
  profilerStopDeadlineMs?: number;
}): Promise<EvidenceResult> {
  const dwellMs = opts.dwellMs ?? 5_000;
  // `Profiler.stop` is main-thread-answered, so on a wedge that never clears it
  // never returns. The deadline is a BOUND on how long we wait for a
  // self-healing one to hand the profile over, not an expectation.
  const profilerStopDeadlineMs = opts.profilerStopDeadlineMs ?? 90_000;
  mkdirSync(opts.dir, { recursive: true });

  // Console first — it costs nothing and it is the one artifact a later hang
  // could otherwise lose.
  writeFileSync(path.join(opts.dir, 'console.log'), `${opts.consoleLines.join('\n')}\n`, 'utf8');

  // Let the already-running profiler collect the wedge window itself.
  await sleep(dwellMs);

  let profileSaved = false;
  let profileError: string | undefined;
  let topFrames: string[] = [];

  // Both captures run concurrently on separate CDP sessions: a `Profiler.stop`
  // that may never answer must not sit in front of the interrupt-path pause,
  // and vice versa.
  const [pausedStacks] = await Promise.all([
    collectPausedStacks(opts.sessions.debug, opts.sessions.scripts, 4, 700, 5_000),
    (async () => {
      try {
        const stopped = (await withDeadline(
          opts.sessions.profiler.send('Profiler.stop'),
          profilerStopDeadlineMs,
          'Profiler.stop',
        )) as { profile: { nodes?: ProfileNode[] } };
        writeFileSync(
          path.join(opts.dir, 'profile.cpuprofile'),
          JSON.stringify(stopped.profile),
          'utf8',
        );
        profileSaved = true;
        topFrames = hottestFrames(stopped.profile, 10);
      } catch (err) {
        profileError = String(err);
      }
    })(),
  ]);

  const firstStack = pausedStacks.find((sample) => sample.callFrames?.length);
  if (firstStack?.callFrames) {
    topFrames = [
      ...firstStack.callFrames
        .slice(0, 10)
        .map(
          (frame) =>
            `paused  ${frame.functionName} @ ${frame.url || `script#${frame.scriptId}`}` +
            `:${frame.lineNumber + 1}:${frame.columnNumber ?? 0}`,
        ),
      ...topFrames,
    ].slice(0, 10);
  }

  writeFileSync(
    path.join(opts.dir, 'stacks.json'),
    JSON.stringify(
      {
        wedge: opts.wedge,
        longestGapMs: opts.monitor.longestGapMs,
        wedgeDurationMs: opts.monitor.recoveredAfterMs,
        stillWedgedAtCapture: opts.monitor.recoveredAfterMs === null,
        probesResolved: opts.monitor.probesResolved,
        // A wedge is read off silence, so the reader has to be able to rule
        // out the other thing that produces it.
        probesRejected: opts.monitor.probesRejected,
        lastRejection: opts.monitor.lastRejection,
        pausedStacks,
        profileSaved,
        profileError,
        topFrames,
      },
      null,
      2,
    ),
    'utf8',
  );

  // Re-write the console: a wedge often coughs up its most useful lines while
  // the capture above was dwelling.
  writeFileSync(path.join(opts.dir, 'console.log'), `${opts.consoleLines.join('\n')}\n`, 'utf8');

  return { dir: opts.dir, topFrames, pausedStacks, profileSaved, profileError };
}
