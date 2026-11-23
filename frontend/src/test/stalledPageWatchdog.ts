// Ends a browser-project run that has stopped reporting.
//
// Test and hook timeouts are timers inside the test page, so a page whose
// main thread is blocked never fires them, and Vitest's own heartbeat is
// answered by the browser's network layer while the page is stuck. Without
// this reporter such a run waits for the CI job's timeout and names nothing.
//
// The reporter sees only public reporter events and keeps two clocks:
//   - each running test file, keyed by project name and module id (Vitest
//     hands the queued event and later events different TestModule
//     objects for one file), from queued until it ends;
//   - the run, from run start to run end, restarted by any event.
// It fires when a running browser file has been silent for the bound, or
// when the run is in flight with no file running, a browser file has run,
// and nothing at all has been reported for the bound (a page that stops
// after its file's last result). It then names what it saw, sets a failing
// exit code and calls `vitest.exit(true)`. `close()` waits for the stuck
// run, so the exit lands after `teardownTimeout`, through `process.exit()`,
// which also stops the Playwright browser.
//
// A file's clock starts at queued, so it also covers importing the file and
// its setup files, which no Vitest timer bounds. Across 10 passing CI runs
// the slowest browser file took 48 s in total, imports included, so a bound
// well above the 60 s test timeout leaves that phase room as well. A test
// that sets its own timeout at or above the bound would be reported as a
// stall while it legitimately runs.
//
// Public reporter events do not show when a browser session has finished,
// so a page that hangs in Vitest's cleanup after its file's last result is
// caught only once every other file has stopped reporting, and the report
// cannot name it: the run still ends, later and without the file name.
import type {
  Reporter,
  ReportedHookContext,
  TestCase,
  TestModule,
  TestProject,
  TestSuite,
  Vitest,
} from 'vitest/node';

type UserConsoleLog = Parameters<NonNullable<Reporter['onUserConsoleLog']>>[0];

export interface StalledPageWatchdogOptions {
  /** Silence that counts as a stalled run or a stalled browser file. */
  boundMs: number;
  /** How often the reporter checks for silence. */
  checkEveryMs?: number;
  now?: () => number;
}

interface RunningFile {
  moduleId: string;
  browser: boolean;
  lastEventAt: number;
}

interface RunState {
  lastEventAt: number;
  browserFileRan: boolean;
  /** Files of this run, by key. */
  running: Map<string, RunningFile>;
  ended: Set<string>;
  /** File key of each module, suite and test id seen, to attribute console logs. */
  taskFile: Map<string, string>;
}

export class StalledPageWatchdog implements Reporter {
  private readonly boundMs: number;
  private readonly checkEveryMs: number;
  private readonly now: () => number;
  private vitest: Vitest | undefined;
  private timer: ReturnType<typeof setInterval> | undefined;
  private run: RunState | undefined;
  private fired = false;

  constructor(options: StalledPageWatchdogOptions) {
    this.boundMs = options.boundMs;
    this.checkEveryMs = options.checkEveryMs ?? 5_000;
    this.now = options.now ?? Date.now;
  }

  get bound(): number {
    return this.boundMs;
  }

  onInit(vitest: Vitest): void {
    this.vitest = vitest;
    // A live page fails a test or hook at its timeout and reports it. A
    // timeout at or above the bound, or none (0), would let a slow live test
    // look stalled.
    for (const project of vitest.projects) {
      if (!isBrowserProject(project)) continue;
      const { testTimeout, hookTimeout } = project.config;
      const bounded = (ms: number) => ms > 0 && ms < this.boundMs;
      if (!bounded(testTimeout) || !bounded(hookTimeout)) {
        throw new Error(
          `stalled-page watchdog: project "${project.name}" has testTimeout ${testTimeout} ms and ` +
            `hookTimeout ${hookTimeout} ms; both must be set (not 0) and below the ${this.boundMs} ms stall bound`,
        );
      }
    }
  }

  onTestRunStart(): void {
    this.run = {
      lastEventAt: this.now(),
      browserFileRan: false,
      running: new Map(),
      ended: new Set(),
      taskFile: new Map(),
    };
    this.fired = false;
    clearInterval(this.timer);
    this.timer = setInterval(() => this.check(), this.checkEveryMs);
    // The run, not this timer, decides when the process may exit.
    this.timer.unref?.();
  }

  onTestRunEnd(): void {
    clearInterval(this.timer);
    this.timer = undefined;
    this.run = undefined;
  }

  onTestModuleQueued(module: TestModule): void {
    this.heard(module);
  }

  onTestModuleCollected(module: TestModule): void {
    this.heard(module);
  }

  onTestModuleStart(module: TestModule): void {
    this.heard(module);
  }

  onTestModuleEnd(module: TestModule): void {
    if (!this.run) return;
    this.run.lastEventAt = this.now();
    const key = fileKey(module);
    this.run.running.delete(key);
    this.run.ended.add(key);
  }

  onHookStart(hook: ReportedHookContext): void {
    this.heard(moduleOf(hook.entity), hook.entity.id);
  }

  onHookEnd(hook: ReportedHookContext): void {
    this.heard(moduleOf(hook.entity), hook.entity.id);
  }

  onTestSuiteReady(suite: TestSuite): void {
    this.heard(suite.module, suite.id);
  }

  onTestSuiteResult(suite: TestSuite): void {
    this.heard(suite.module, suite.id);
  }

  onTestCaseReady(test: TestCase): void {
    this.heard(test.module, test.id);
  }

  onTestCaseResult(test: TestCase): void {
    this.heard(test.module, test.id);
  }

  onTestCaseAnnotate(test: TestCase): void {
    this.heard(test.module, test.id);
  }

  onTestCaseArtifactRecord(test: TestCase): void {
    this.heard(test.module, test.id);
  }

  // A console log proves its page runs JavaScript. It refreshes the file of
  // the task it names, or every running browser file when it names none
  // this reporter has seen.
  onUserConsoleLog(log: UserConsoleLog): void {
    const run = this.run;
    if (!run) return;
    const now = this.now();
    run.lastEventAt = now;
    const key = log.taskId === undefined ? undefined : run.taskFile.get(log.taskId);
    if (key !== undefined) {
      const file = run.running.get(key);
      if (file) file.lastEventAt = now;
      return;
    }
    for (const file of run.running.values()) if (file.browser) file.lastEventAt = now;
  }

  private heard(module: TestModule, taskId?: string): void {
    const run = this.run;
    if (!run) return;
    const now = this.now();
    run.lastEventAt = now;
    const key = fileKey(module);
    run.taskFile.set(module.id, key);
    if (taskId !== undefined) run.taskFile.set(taskId, key);
    // An event that arrives after the file ended must not revive it.
    if (run.ended.has(key)) return;
    const browser = isBrowserProject(module.project);
    run.running.set(key, { moduleId: module.moduleId, browser, lastEventAt: now });
    if (browser) run.browserFileRan = true;
  }

  private check(): void {
    const run = this.run;
    if (this.fired || !this.vitest || !run) return;
    const now = this.now();
    const seconds = (ms: number) => Math.round(ms / 1000);
    const bound = `${seconds(this.boundMs)} s`;
    const silent = [...run.running.values()].filter((file) => file.browser && now - file.lastEventAt >= this.boundMs);
    let report: string | undefined;
    if (silent.length > 0) {
      const lines = silent.map((file) => `  ${file.moduleId} (no report for ${seconds(now - file.lastEventAt)} s)`);
      report =
        `these running browser test files have reported nothing for at least ${bound}:\n${lines.join('\n')}\n` +
        'This usually means their page stopped running JavaScript: a live page fails any test or hook at its ' +
        'timeout and reports it.';
    } else if (run.running.size === 0 && run.browserFileRan && now - run.lastEventAt >= this.boundMs) {
      report =
        `the run has reported nothing for ${seconds(now - run.lastEventAt)} s (bound ${bound}) with no test file ` +
        'running, and has not ended.\nThis usually means a browser page stopped running JavaScript after its ' +
        "file's last result, so the run waits on it.";
    }
    if (report === undefined) return;
    this.fired = true;
    clearInterval(this.timer);
    this.vitest.logger.error(`\nstalled-page watchdog: ${report}\nEnding the run with a failure.\n`);
    process.exitCode = 1;
    void this.vitest.exit(true).catch((error: unknown) => {
      this.vitest?.logger.error('stalled-page watchdog: vitest.exit failed', error);
      process.exit(1);
    });
  }
}

function fileKey(module: TestModule): string {
  return `${module.project.name}\0${module.moduleId}`;
}

function isBrowserProject(project: TestProject): boolean {
  return project.config.browser.enabled === true;
}

function moduleOf(entity: TestModule | TestSuite | TestCase): TestModule {
  return entity.type === 'module' ? entity : entity.module;
}
