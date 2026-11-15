import type { ChildProcess } from 'node:child_process';

// Every step of starting a process the suite owns runs under its own bound,
// so a stall fails naming the step instead of running into a Playwright
// fixture or test timeout that names nothing.

/** The bound of every launchHarness step except the bootstrap line. */
export const LAUNCH_STEP_MS = 5_000;
/** The bootstrap line's default bound; first boot migrates the database. */
export const BOOTSTRAP_STEP_MS = 30_000;
/** The bound on stopping and removing a launch that failed. */
export const LAUNCH_CLEANUP_MS = 25_000;
/** launchHarness's steps bounded by LAUNCH_STEP_MS, besides the bootstrap line. */
const LAUNCH_STEPS = [
  'create data dir',
  'identify process',
  'capture process proofs',
  'refresh process proofs',
  'connect',
  'start watchdog',
];

/**
 * The longest launchHarness can take to fail, cleanup included. A fixture
 * that launches a backend needs a timeout above it, or its timeout fires
 * first and the step that stalled goes unnamed.
 */
export function launchBudgetMs(bootstrapMs = BOOTSTRAP_STEP_MS): number {
  return LAUNCH_STEPS.length * LAUNCH_STEP_MS + bootstrapMs + LAUNCH_CLEANUP_MS;
}

/**
 * Runs one launch step under `boundMs`. A step that has not settled by then
 * fails with `harness launch: <step> did not finish in <n> s` and whatever
 * `diagnose` reports. The step's own work is not cancelled; the caller's
 * failure path stops the process it belongs to.
 */
export async function launchStep<T>(
  step: string,
  boundMs: number,
  work: () => Promise<T>,
  diagnose: () => string = () => '',
): Promise<T> {
  const operation = work();
  // The step may settle after its bound fired; that late outcome has no reader.
  void operation.catch(() => undefined);
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      operation,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => {
          const detail = diagnose();
          reject(new Error(`harness launch: ${step} did not finish in ${boundMs / 1000} s${detail ? `\n${detail}` : ''}`));
        }, boundMs);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

/** How many stderr lines a launch failure quotes. */
const DIAGNOSTIC_LINES = 20;

/**
 * What a stalled launch step can report about its process without waiting
 * on anything: whether it is still running, and the end of its stderr. The
 * full tail rides the test's `backend <pid> stderr` attachment.
 */
export function processDiagnostics(
  name: string,
  child: ChildProcess,
  stderrLines: () => readonly string[],
  details: readonly string[] = [],
): () => string {
  return () => {
    const state =
      child.exitCode !== null
        ? `exited with code ${child.exitCode}`
        : child.signalCode !== null
          ? `killed by ${child.signalCode}`
          : 'alive';
    const tail = stderrLines().slice(-DIAGNOSTIC_LINES);
    return [
      `${name} pid ${child.pid ?? 'unknown'}: ${state}`,
      ...details,
      tail.length === 0 ? `${name} stderr: empty` : `${name} stderr, last ${tail.length} lines:`,
      ...tail,
    ].join('\n');
  };
}
