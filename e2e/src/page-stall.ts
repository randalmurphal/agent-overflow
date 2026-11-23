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
// also reads page state between requests keeps reaching them. `armDebugger`
// therefore runs while the page is healthy, with breakpoints deactivated so `debugger;`
// statements in page code never stop it. While paused, the inspector answers
// `Debugger.getScriptSource`, which gives each frame its bundle text.
//
// tests/base-test.ts, which every spec builds on, arms every page its worker
// opens, a `beforeAll`'s included, and pauses the open ones that do not
// answer when a test fails;
// tests/freeze-repro-probe.ts arms its replay page and samples the stack of
// a wedge. What the failure capture does not cover:
//   - a page in a context the test closed itself before failing (an expect
//     that timed out, then a finally that closed it): it is gone;
//   - a page on a browser connection of its own (`chromium.launch`,
//     `connectOverCDP`, as tests/browser-lifecycle-helpers.ts and
//     tests/harness-passkey-lifecycle.spec.ts do), and manual specs, which
//     keep their own setup;
//   - a page that stalls before its session is armed. Arming starts from the
//     page event, usually done within milliseconds; the `page` fixture waits
//     up to STALL_ARM_MS for it and then hands the page over armed or not,
//     and a popup cannot be armed before its first document runs. The
//     capture reports a page it could not arm as not armed, without a stack.

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

/**
 * The limits of the failure capture. Pages are captured concurrently, so a
 * failure gets at most PROBE + PAUSE + SOURCE + RESUME (11 s) longer, and
 * only when a page does not answer the probe. The capture fixtures get their
 * own timeout, `STALL_CAPTURE_TIMEOUT_MS`, so after-hooks that spent the
 * shared teardown time cannot cut a capture short.
 */
export const STALL_CAPTURE_BOUNDS = {
  /** A trivial evaluate a running page answers at once. */
  probeMs: 2_000,
  /** `Debugger.pause` lands within 300 ms when it can land at all. */
  pauseMs: 5_000,
  /** Reading the scripts the top frames sit in, while paused. */
  sourceMs: 3_000,
  resumeMs: 1_000,
} as const;

/** How long the page fixture waits for a page's session to arm before handing it over. */
export const STALL_ARM_MS = 2_000;

export const STALL_CAPTURE_TIMEOUT_MS =
  Object.values(STALL_CAPTURE_BOUNDS).reduce((sum, ms) => sum + ms, 0) + 5_000;

/** The memory limits of an armed session and of one capture. */
export const STALL_CAPTURE_LIMITS = {
  /** Script urls kept per armed session; the oldest go first. */
  scriptRecords: 2_000,
  /** Characters of a script url kept. */
  scriptUrlChars: 300,
  /**
   * Characters of script source fetched per failure, across all stalled
   * pages. Each script is fetched once however many frames point into it;
   * one that does not fit what is left is not fetched.
   */
  sourceCharsPerFailure: 8 * 2 ** 20,
} as const;

/** What `Debugger.getScriptSource` gave for each script id. */
export type SourceCache = Map<string, { source: string } | { error: string }>;

/** Source characters a capture may still fetch; shared by the pages of one failure. */
export interface SourceBudget {
  remainingChars: number;
}

interface WirePausedFrame {
  functionName: string;
  url?: string;
  location: { scriptId: string; lineNumber: number; columnNumber?: number };
}

interface PausedEvent {
  callFrames: WirePausedFrame[];
}

/**
 * The one owner of a session's pause state. Every `Debugger.paused` goes to
 * the pause request waiting for it; one that arrives with none waiting (a
 * pause that landed after its request gave up) is resumed at once, and a
 * failure to resume it is kept for the next request to report.
 */
class PauseOwner {
  private waiter: ((event: PausedEvent) => void) | undefined;
  private lateResumeErrors: string[] = [];
  /** The pause request in flight; the next one starts after it settles. */
  private inFlight: Promise<unknown> = Promise.resolve();

  constructor(private readonly session: CDPSession) {
    session.on('Debugger.paused', (event) => this.paused(event as PausedEvent));
  }

  private paused(event: PausedEvent): void {
    const waiter = this.waiter;
    this.waiter = undefined;
    if (waiter) {
      waiter(event);
      return;
    }
    this.session.send('Debugger.resume').catch((err: unknown) => {
      const message = `resuming a pause that landed after its request gave up failed: ${errorText(err)}`;
      this.lateResumeErrors.push(message);
      console.error(`page-stall: ${message}`);
    });
  }

  /**
   * Run one pause request after the one in flight has finished, so two
   * requests never wait on the same `Debugger.paused` or resume each
   * other's pause.
   */
  exclusive<T>(request: () => Promise<T>): Promise<T> {
    const run = this.inFlight.then(request);
    this.inFlight = run.catch(() => undefined);
    return run;
  }

  /** Wait for the next pause. Called only inside `exclusive`. */
  next(): Promise<PausedEvent> {
    return new Promise((resolve) => {
      this.waiter = resolve;
    });
  }

  retire(): void {
    this.waiter = undefined;
  }

  takeLateResumeErrors(): string[] {
    const errors = this.lateResumeErrors;
    this.lateResumeErrors = [];
    return errors;
  }
}

interface ScriptRecord {
  url: string;
  /** Source length in characters, as Debugger.scriptParsed reports it; absent when it reports none. */
  length?: number;
}

export interface ArmedDebugger {
  session: CDPSession;
  /** Scripts with a url, by scriptId, newest last, capped at STALL_CAPTURE_LIMITS.scriptRecords. */
  scripts: Map<string, ScriptRecord>;
  pauses: PauseOwner;
}

const armedSessions = new WeakMap<CDPSession, Promise<ArmedDebugger>>();

/**
 * Enable the Debugger domain on a page session while the page still answers.
 * A session has one owner of its pauses: arming it again returns the first
 * arming. One whose arming failed can be armed again.
 */
export function armDebugger(session: CDPSession): Promise<ArmedDebugger> {
  const existing = armedSessions.get(session);
  if (existing) return existing;
  const armed = arm(session);
  armedSessions.set(session, armed);
  armed.catch(() => {
    if (armedSessions.get(session) === armed) armedSessions.delete(session);
  });
  return armed;
}

async function arm(session: CDPSession): Promise<ArmedDebugger> {
  const scripts = new Map<string, ScriptRecord>();
  session.on('Debugger.scriptParsed', (event) => {
    // Frames need a url only for scripts that have one; eval'd and inline
    // scripts are most of what parses and are left out.
    if (!event.scriptId || !event.url) return;
    scripts.set(event.scriptId, { url: event.url.slice(0, STALL_CAPTURE_LIMITS.scriptUrlChars), length: event.length });
    if (scripts.size > STALL_CAPTURE_LIMITS.scriptRecords) scripts.delete(scripts.keys().next().value!);
  });
  const pauses = new PauseOwner(session);
  await session.send('Debugger.enable');
  await session.send('Debugger.setBreakpointsActive', { active: false });
  return { session, scripts, pauses };
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

export type PausedStack = (
  | { paused: true; pausedAfterMs: number; frames: StackFrame[]; resumeError?: string }
  | {
      paused: false;
      error: string;
      /** What became of the `Debugger.pause` command's own reply. */
      pauseReply: string;
    }
) & {
  /** Failures to resume pauses that landed after an earlier request gave up. */
  earlierResumeErrors: string[];
};

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
  /**
   * What fetching each script gave, its source or its error, so a script is
   * fetched at most once for as long as the caller keeps the cache.
   */
  sourceCache?: SourceCache;
  /** Defaults to STALL_CAPTURE_LIMITS.sourceCharsPerFailure for this call alone. */
  sourceBudget?: SourceBudget;
}

/**
 * Pause the page, read its call stack and the source around the top frames,
 * then resume it. Every step is bounded; a step that fails is reported in
 * the result, never thrown. A call made while another on the same session
 * is in flight starts when that one has finished.
 */
export function pauseAndReadStack(armed: ArmedDebugger, options: PauseOptions): Promise<PausedStack> {
  return armed.pauses.exclusive(() => pauseAndRead(armed, options));
}

async function pauseAndRead(armed: ArmedDebugger, options: PauseOptions): Promise<PausedStack> {
  const { session, scripts, pauses } = armed;
  const sourceFrames = options.sourceFrames ?? 8;
  const radius = options.sourceRadius ?? 160;
  const cache: SourceCache = options.sourceCache ?? new Map();
  const budget = options.sourceBudget ?? { remainingChars: STALL_CAPTURE_LIMITS.sourceCharsPerFailure };
  const earlierResumeErrors = pauses.takeLateResumeErrors();
  const startedAt = Date.now();

  // Wait before the command: a blocked thread can deliver `paused` the
  // instant the interrupt lands.
  const paused = pauses.next();
  let event: PausedEvent;
  let pauseReply = `not answered within ${options.pauseMs} ms`;
  try {
    // Wait for the event, not the command's reply: the reply can trail the
    // pause. A command that fails (no Debugger domain) still ends the wait.
    const refused = session.send('Debugger.pause').then(
      () => {
        pauseReply = `answered after ${Date.now() - startedAt} ms`;
        return new Promise<never>(() => undefined);
      },
      (err: unknown) => {
        pauseReply = `failed after ${Date.now() - startedAt} ms: ${errorText(err)}`;
        throw err;
      },
    );
    event = await withDeadline(Promise.race([paused, refused]), options.pauseMs, 'Debugger.paused');
  } catch (err) {
    pauses.retire();
    return { paused: false, error: errorText(err), pauseReply, earlierResumeErrors };
  }
  const pausedAfterMs = Date.now() - startedAt;

  const sourceDeadlineAt = Date.now() + options.sourceMs;
  const frames: StackFrame[] = [];
  for (const [depth, frame] of event.callFrames.entries()) {
    const columnNumber = frame.location.columnNumber ?? 0;
    const out: StackFrame = {
      functionName: frame.functionName || '(anonymous)',
      url: frame.url || scripts.get(frame.location.scriptId)?.url || '',
      scriptId: frame.location.scriptId,
      lineNumber: frame.location.lineNumber,
      columnNumber,
    };
    if (depth < sourceFrames) {
      try {
        const source = await scriptSource(armed, cache, budget, out.scriptId, sourceDeadlineAt - Date.now());
        out.source = snippet(source, out.lineNumber, columnNumber, radius);
      } catch (err) {
        out.sourceError = errorText(err);
      }
    }
    frames.push(out);
  }

  try {
    await withDeadline(session.send('Debugger.resume'), options.resumeMs, 'Debugger.resume');
    return { paused: true, pausedAfterMs, frames, earlierResumeErrors };
  } catch (err) {
    return { paused: true, pausedAfterMs, frames, resumeError: errorText(err), earlierResumeErrors };
  }
}

/**
 * A script's source, fetched only when its reported size fits the budget.
 * The fetch's outcome, source or error, is cached; a refusal before fetching
 * is not, as a later sample may have the time or the size record.
 */
async function scriptSource(
  armed: ArmedDebugger,
  cache: SourceCache,
  budget: SourceBudget,
  scriptId: string,
  remainingMs: number,
): Promise<string> {
  const cached = cache.get(scriptId);
  if (cached !== undefined) {
    if ('error' in cached) throw new Error(cached.error);
    return cached.source;
  }
  if (remainingMs <= 0) throw new Error('the source time budget was spent on earlier frames');
  const record = armed.scripts.get(scriptId);
  if (record === undefined) throw new Error('the script has no url record, so its size is unknown and it is not fetched');
  if (record.length === undefined) throw new Error('the script\'s size was not reported when it parsed, so it is not fetched');
  if (record.length > budget.remainingChars) {
    throw new Error(
      `the script is ${record.length} characters and ${budget.remainingChars} of this failure's source budget are left`,
    );
  }
  // Charge the reported size now, so pages fetching at once share the
  // budget, and settle to what was read once it arrives.
  budget.remainingChars -= record.length;
  let source: string;
  try {
    ({ scriptSource: source } = await withDeadline(
      armed.session.send('Debugger.getScriptSource', { scriptId }),
      remainingMs,
      'Debugger.getScriptSource',
    ));
  } catch (err) {
    // Nothing was read, so nothing is charged.
    budget.remainingChars += record.length;
    const error = `Debugger.getScriptSource failed: ${errorText(err)}`;
    cache.set(scriptId, { error });
    throw new Error(error);
  }
  budget.remainingChars += record.length - source.length;
  cache.set(scriptId, { source });
  return source;
}

/** The text around one position, found without splitting the whole script. */
function snippet(source: string, lineNumber: number, columnNumber: number, radius: number): string {
  let lineStart = 0;
  for (let line = 0; line < lineNumber; line += 1) {
    const next = source.indexOf('\n', lineStart);
    if (next < 0) throw new Error(`the script has no line ${lineNumber + 1}`);
    lineStart = next + 1;
  }
  const end = source.indexOf('\n', lineStart);
  const lineEnd = end < 0 ? source.length : end;
  const from = Math.max(lineStart, lineStart + columnNumber - radius);
  const to = Math.min(lineEnd, lineStart + columnNumber + radius);
  return `${from > lineStart ? '…' : ''}${source.slice(from, to)}${to < lineEnd ? '…' : ''}`;
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

type ArmResult = { armed: ArmedDebugger } | { error: string };

/**
 * A worker's open browser contexts and their pages, each page armed when it
 * opens. Contexts leave it when they close. Every test's capture reads it,
 * so a page a `beforeAll` opened is captured like one the test opened.
 */
export class StalledPageRegistry {
  private readonly contexts = new Set<BrowserContext>();
  private readonly armed = new WeakMap<Page, Promise<ArmResult>>();
  private readonly armedNow = new WeakSet<Page>();
  /** The last thing each page did that reaches the test without its main thread's help. */
  private readonly lastActivity = new WeakMap<Page, { what: string; at: number }>();

  /** Call for each browser context the worker's browser creates. */
  trackContext(context: BrowserContext): void {
    this.contexts.add(context);
    context.on('close', () => this.contexts.delete(context));
    context.on('page', (page) => {
      this.trackActivity(page);
      this.armPage(context, page);
    });
  }

  private trackActivity(page: Page): void {
    const saw = (what: string) => this.lastActivity.set(page, { what: what.slice(0, 300), at: Date.now() });
    saw('opened');
    page.on('request', (request) => saw(`request ${request.method()} ${request.url()}`));
    page.on('console', (message) => saw(`console.${message.type()}`));
    page.on('framenavigated', (frame) => {
      if (frame === page.mainFrame()) saw(`navigated to ${frame.url()}`);
    });
  }

  private armPage(context: BrowserContext, page: Page): void {
    if (!hasCdp(context)) return;
    const armed = (async (): Promise<ArmResult> => {
      try {
        const result = { armed: await armDebugger(await context.newCDPSession(page)) };
        this.armedNow.add(page);
        return result;
      } catch (err) {
        return { error: errorText(err) };
      }
    })();
    this.armed.set(page, armed);
  }

  /** Whether the page's debugger session is armed at this moment. */
  isArmed(page: Page): boolean {
    return this.armedNow.has(page);
  }

  /**
   * Wait, at most STALL_ARM_MS, until the page's session is armed. A page
   * that is not armed by then is reported as such by the capture, not here.
   */
  async untilArmed(page: Page): Promise<void> {
    const armed = this.armed.get(page);
    if (armed) await withDeadline(armed, STALL_ARM_MS, 'arming the debugger session').catch(() => undefined);
  }

  /** Every open page of the worker's open contexts. */
  openPages(): Page[] {
    return [...this.contexts].flatMap((context) => context.pages()).filter((page) => !page.isClosed());
  }

  /**
   * The report for one page that does not answer the probe, or undefined
   * for one that does. Every step is bounded and none throws: a step that
   * fails says what it tried and its error in the report.
   */
  async capturePage(
    page: Page,
    index: number,
    count: number,
    budget: SourceBudget,
    sourceCache: SourceCache,
  ): Promise<string | undefined> {
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
      `The page did not answer a trivial evaluate (page.evaluate) within ${bounds.probeMs} ms, so its main thread is not running this test's work.`,
    ];
    const activity = this.lastActivity.get(page);
    if (activity) {
      lines.push(
        `Last thing the page did that the test saw (a request, a console message or a main-frame navigation): ` +
          `${activity.what}, ${Date.now() - activity.at} ms before the probe gave up.`,
      );
    }
    if (!ready) {
      lines.push(
        hasCdp(page.context())
          ? 'No debugger session was armed for this page (its context was not created through the worker\'s browser), so its stack cannot be read.'
          : `This browser (${page.context().browser()?.browserType().name() ?? 'unknown'}) has no CDP, so its stack cannot be read.`,
      );
      return lines.join('\n');
    }
    if ('error' in ready) {
      lines.push(`The debugger session for this page was not armed (${ready.error}), so its stack cannot be read.`);
      return lines.join('\n');
    }
    const stack = await pauseAndReadStack(ready.armed, { ...bounds, sourceBudget: budget, sourceCache });
    for (const error of stack.earlierResumeErrors) lines.push(`Before this capture: ${error}`);
    if (!stack.paused) {
      lines.push(
        `Debugger.pause did not land within ${bounds.pauseMs} ms. V8 takes a pause only at its own ` +
          'interrupt checks: the main thread is either outside JavaScript (layout, a native call, the renderer ' +
          'itself) or in a loop of native calls that reaches no check, such as back-to-back synchronous requests.',
        'CDP calls tried on the debugger session armed when the page opened:',
        `  Debugger.pause: reply ${stack.pauseReply}`,
        `  Debugger.paused event: ${stack.error}`,
        '  A pause that lands later is resumed at once.',
      );
      return lines.join('\n');
    }
    lines.push(`JavaScript call stack, paused ${stack.pausedAfterMs} ms after asking (bundles are minified; names are as shipped):`);
    lines.push(formatStack(stack.frames));
    if (stack.resumeError) lines.push(`Debugger.resume failed after the capture: ${stack.resumeError}`);
    return lines.join('\n');
  }
}

/** One test's view of its worker's registry, capturing at most once. */
export class StalledPageCapture {
  private captured = false;

  constructor(private readonly registry: StalledPageRegistry) {}

  /** Whether the page's debugger session is armed at this moment. */
  isArmed(page: Page): boolean {
    return this.registry.isArmed(page);
  }

  /** See StalledPageRegistry.untilArmed. */
  untilArmed(page: Page): Promise<void> {
    return this.registry.untilArmed(page);
  }

  /**
   * When the test failed, probe every page open in the worker at this
   * moment and attach a report for each that does not answer. Runs once per
   * test; later calls return at once.
   */
  async captureIfFailed(testInfo: TestInfo): Promise<void> {
    // A later teardown can still fail the test, so only a capture that ran
    // closes the door.
    if (this.captured || testInfo.status === 'skipped' || testInfo.status === testInfo.expectedStatus) return;
    this.captured = true;
    const pages = this.registry.openPages();
    // One budget and one cache for the whole failure.
    const budget: SourceBudget = { remainingChars: STALL_CAPTURE_LIMITS.sourceCharsPerFailure };
    const cache: SourceCache = new Map();
    const reports = await Promise.all(
      pages.map((page, index) => this.registry.capturePage(page, index, pages.length, budget, cache)),
    );
    for (const report of reports) {
      if (report === undefined) continue;
      await testInfo.attach('stalled page', { body: report, contentType: 'text/plain' });
    }
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
