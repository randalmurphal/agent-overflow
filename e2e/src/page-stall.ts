// Reading the JavaScript call stack of a page whose main thread has stopped
// answering, through the Chrome DevTools Protocol.
//
// A blocked main thread queues every evaluate and every main-thread-answered
// CDP command (`Debugger.enable`, `Profiler.stop`) behind itself, so a session
// attached after the block answers nothing. `Debugger.pause` rides V8's
// interrupt path and lands while the thread is blocked, but only on a session
// whose Debugger domain was enabled before the block, and only when the
// blocked code reaches one of V8's interrupt checks: a loop of back-to-back
// synchronous requests stops reaching them a few seconds in, while one that
// also reads page state between requests keeps reaching them. `armDebugger` therefore
// runs while the page is healthy, with breakpoints deactivated so `debugger;`
// statements in page code never stop it. While paused, the inspector answers
// `Debugger.getScriptSource`, which gives each frame its bundle text.
//
// tests/fixtures.ts arms every page a test opens and pauses the ones that do
// not answer when the test fails; tests/freeze-repro-probe.ts arms its replay
// page and samples the stack of a wedge.

import type { BrowserContext, CDPSession, Page, TestInfo } from '@playwright/test';

/** Reject rather than hang forever when the renderer never answers. */
export function withDeadline<T>(promise: Promise<T>, ms: number, what: string): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`${what} did not answer within ${ms} ms`)), ms);
    promise.then(
      (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      (err: unknown) => {
        clearTimeout(timer);
        reject(err instanceof Error ? err : new Error(String(err)));
      },
    );
  });
}

export function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export interface ArmedDebugger {
  session: CDPSession;
  /** scriptId to url, from Debugger.scriptParsed, for frames that carry no url. */
  scripts: Map<string, string>;
}

/** Enable the Debugger domain on a page session while the page still answers. */
export async function armDebugger(session: CDPSession): Promise<ArmedDebugger> {
  const scripts = new Map<string, string>();
  session.on('Debugger.scriptParsed', (event) => {
    if (event.scriptId) scripts.set(event.scriptId, event.url ?? '');
  });
  await session.send('Debugger.enable');
  await session.send('Debugger.setBreakpointsActive', { active: false });
  return { session, scripts };
}

export interface StackFrame {
  functionName: string;
  url: string;
  scriptId: string;
  /** Zero-based, as CDP reports it. */
  lineNumber: number;
  columnNumber: number;
  /**
   * The bundle text around the frame's position. Bundles are minified, so a
   * name and a position alone say little; the source around it is what makes
   * a stack readable.
   */
  source?: string;
  /** Why the source could not be read, when it could not. */
  sourceError?: string;
}

export type PausedStack =
  | { paused: true; pausedAfterMs: number; frames: StackFrame[]; resumeError?: string }
  | { paused: false; error: string };

export interface PauseOptions {
  /** How long to wait for `Debugger.pause` to land. */
  pauseMs: number;
  /** Total time for reading frame sources while paused, across all frames. */
  sourceMs: number;
  /** How long to wait for `Debugger.resume`. */
  resumeMs: number;
  /** Frames from the top that get a source snippet. */
  sourceFrames?: number;
  /** Characters of source kept on each side of a frame's position. */
  sourceRadius?: number;
  /** Scripts already fetched, shared across samples of one page. */
  sourceCache?: Map<string, string[]>;
}

interface WirePausedFrame {
  functionName: string;
  url?: string;
  location: { scriptId: string; lineNumber: number; columnNumber?: number };
}

/**
 * Pause the page, read its call stack and the source around the top frames,
 * then resume it. Every step is bounded; a step that fails is reported in
 * the result, never thrown.
 */
export async function pauseAndReadStack(armed: ArmedDebugger, options: PauseOptions): Promise<PausedStack> {
  const { session, scripts } = armed;
  const sourceFrames = options.sourceFrames ?? 8;
  const radius = options.sourceRadius ?? 160;
  const cache = options.sourceCache ?? new Map<string, string[]>();
  const startedAt = Date.now();

  // Register the listener before the command: a blocked thread can deliver
  // `paused` the instant the interrupt lands.
  let onPaused: ((event: { callFrames: WirePausedFrame[] }) => void) | undefined;
  const paused = new Promise<{ callFrames: WirePausedFrame[] }>((resolve) => {
    onPaused = resolve;
    session.once('Debugger.paused', resolve);
  });
  let event: { callFrames: WirePausedFrame[] };
  try {
    // Wait for the event, not the command's reply: the reply can trail the
    // pause. A command that fails (no Debugger domain) still ends the wait.
    const refused = session.send('Debugger.pause').then(() => new Promise<never>(() => undefined));
    event = await withDeadline(Promise.race([paused, refused]), options.pauseMs, 'Debugger.paused');
  } catch (err) {
    if (onPaused) session.off('Debugger.paused', onPaused);
    // A pause that lands after the bound would leave the page stopped. The
    // caller has its answer by then; if this resume fails, the page's
    // context closes it anyway.
    session.once('Debugger.paused', () => {
      session.send('Debugger.resume').catch(() => undefined);
    });
    return { paused: false, error: errorText(err) };
  }
  const pausedAfterMs = Date.now() - startedAt;

  const sourceDeadlineAt = Date.now() + options.sourceMs;
  const frames: StackFrame[] = [];
  for (const [depth, frame] of event.callFrames.entries()) {
    const columnNumber = frame.location.columnNumber ?? 0;
    const out: StackFrame = {
      functionName: frame.functionName || '(anonymous)',
      url: frame.url || scripts.get(frame.location.scriptId) || '',
      scriptId: frame.location.scriptId,
      lineNumber: frame.location.lineNumber,
      columnNumber,
    };
    if (depth < sourceFrames) {
      try {
        out.source = await sourceAround(session, cache, out, radius, sourceDeadlineAt - Date.now());
      } catch (err) {
        out.sourceError = errorText(err);
      }
    }
    frames.push(out);
  }

  try {
    await withDeadline(session.send('Debugger.resume'), options.resumeMs, 'Debugger.resume');
    return { paused: true, pausedAfterMs, frames };
  } catch (err) {
    return { paused: true, pausedAfterMs, frames, resumeError: errorText(err) };
  }
}

async function sourceAround(
  session: CDPSession,
  cache: Map<string, string[]>,
  frame: StackFrame,
  radius: number,
  remainingMs: number,
): Promise<string> {
  let lines = cache.get(frame.scriptId);
  if (!lines) {
    if (remainingMs <= 0) throw new Error('the source budget was spent on earlier frames');
    const result = await withDeadline(
      session.send('Debugger.getScriptSource', { scriptId: frame.scriptId }),
      remainingMs,
      'Debugger.getScriptSource',
    );
    lines = result.scriptSource.split('\n');
    cache.set(frame.scriptId, lines);
  }
  const line = lines[frame.lineNumber];
  if (line === undefined) throw new Error(`the script has no line ${frame.lineNumber + 1}`);
  const from = Math.max(0, frame.columnNumber - radius);
  const to = frame.columnNumber + radius;
  return `${from > 0 ? '…' : ''}${line.slice(from, to)}${to < line.length ? '…' : ''}`;
}

/** The stack as text, one frame per line with its source snippet below it. */
export function formatStack(frames: readonly StackFrame[]): string {
  return frames
    .map((frame, depth) => {
      const at = `${frame.url || `script#${frame.scriptId}`}:${frame.lineNumber + 1}:${frame.columnNumber + 1}`;
      const head = `  #${depth} ${frame.functionName} @ ${at}`;
      if (frame.source !== undefined) return `${head}\n      ${frame.source}`;
      if (frame.sourceError !== undefined) return `${head}\n      (source not read: ${frame.sourceError})`;
      return head;
    })
    .join('\n');
}

/**
 * The bounds of the failure capture, per failed test. Pages are captured
 * concurrently, so a failure gets at most PROBE + PAUSE + SOURCE + RESUME
 * (11 s) longer, and only when a page does not answer the probe.
 */
export const STALL_CAPTURE_BOUNDS = {
  /** A trivial evaluate a running page answers at once. */
  probeMs: 2_000,
  /** `Debugger.pause` landed in 1.7 to 3.7 s on a blocked page. */
  pauseMs: 5_000,
  /** Reading the scripts the top frames sit in, while paused. */
  sourceMs: 3_000,
  resumeMs: 1_000,
} as const;

type ArmResult = { armed: ArmedDebugger } | { error: string };

/**
 * Arms every page a test opens and, when the test fails, reports each open
 * page that has stopped running JavaScript.
 */
export class StalledPageCapture {
  private readonly contexts = new Set<BrowserContext>();
  private readonly armed = new WeakMap<Page, Promise<ArmResult>>();
  private captured = false;

  /** Call for each browser context created while the test runs. */
  trackContext(context: BrowserContext): void {
    this.contexts.add(context);
    context.on('close', () => this.contexts.delete(context));
    context.on('page', (page) => this.armPage(context, page));
  }

  private armPage(context: BrowserContext, page: Page): void {
    if (!hasCdp(context)) return;
    const armed = (async (): Promise<ArmResult> => {
      try {
        return { armed: await armDebugger(await context.newCDPSession(page)) };
      } catch (err) {
        return { error: errorText(err) };
      }
    })();
    this.armed.set(page, armed);
  }

  /**
   * Probe every open page the test's contexts hold and attach the stack of
   * each that does not answer. Runs once per test; later calls return at
   * once. Every step is bounded and none throws: a step that fails says
   * what it tried and its error in the attachment.
   */
  async captureIfFailed(testInfo: TestInfo): Promise<void> {
    if (this.captured) return;
    this.captured = true;
    if (testInfo.status === 'skipped' || testInfo.status === testInfo.expectedStatus) return;
    const pages = [...this.contexts].flatMap((context) => context.pages()).filter((page) => !page.isClosed());
    const reports = await Promise.all(pages.map((page, index) => this.capturePage(page, index, pages.length)));
    for (const report of reports) {
      if (report === undefined) continue;
      await testInfo.attach('stalled page', { body: report, contentType: 'text/plain' });
    }
  }

  private async capturePage(page: Page, index: number, count: number): Promise<string | undefined> {
    const bounds = STALL_CAPTURE_BOUNDS;
    // The session was armed when the page opened; waiting for it shares the
    // probe's window so the bound stays as stated.
    const armed = this.armed.get(page);
    const [answer, ready] = await Promise.all([
      probe(page, bounds.probeMs),
      armed
        ? withDeadline(armed, bounds.probeMs, 'arming the debugger session').catch(
            (err: unknown): ArmResult => ({ error: errorText(err) }),
          )
        : undefined,
    ]);
    // A page that answers is running. One that rejects the probe has closed,
    // crashed or navigated, which the failure reports by other means.
    if (answer !== 'silent') return undefined;
    const lines = [
      `page ${index + 1} of ${count}: ${page.url()}`,
      `The page did not answer a trivial evaluate within ${bounds.probeMs} ms, so its main thread is not running this test's work.`,
    ];
    if (!ready) {
      lines.push(
        hasCdp(page.context())
          ? 'No debugger session was armed for this page (it was not opened while the test ran), so its stack cannot be read.'
          : `This browser (${page.context().browser()?.browserType().name() ?? 'unknown'}) has no CDP, so its stack cannot be read.`,
      );
      return lines.join('\n');
    }
    if ('error' in ready) {
      lines.push(`The debugger session for this page was not armed (${ready.error}), so its stack cannot be read.`);
      return lines.join('\n');
    }
    const stack = await pauseAndReadStack(ready.armed, bounds);
    if (!stack.paused) {
      lines.push(
        `Debugger.pause did not land within ${bounds.pauseMs} ms (${stack.error}). V8 takes a pause only at its own ` +
          'interrupt checks: the main thread is either outside JavaScript (layout, a native call, the renderer ' +
          'itself) or in a loop of native calls that reaches no check, such as back-to-back synchronous requests.',
      );
      return lines.join('\n');
    }
    lines.push(`JavaScript call stack, paused ${stack.pausedAfterMs} ms after asking (bundles are minified; names are as shipped):`);
    lines.push(formatStack(stack.frames));
    if (stack.resumeError) lines.push(`Debugger.resume failed after the capture: ${stack.resumeError}`);
    return lines.join('\n');
  }
}

async function probe(page: Page, ms: number): Promise<'answered' | 'rejected' | 'silent'> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const silent = new Promise<'silent'>((resolve) => {
    timer = setTimeout(() => resolve('silent'), ms);
  });
  const answer = page.evaluate(() => 0).then(
    () => 'answered' as const,
    () => 'rejected' as const,
  );
  try {
    return await Promise.race([answer, silent]);
  } finally {
    clearTimeout(timer);
  }
}

function hasCdp(context: BrowserContext): boolean {
  return context.browser()?.browserType().name() === 'chromium';
}
