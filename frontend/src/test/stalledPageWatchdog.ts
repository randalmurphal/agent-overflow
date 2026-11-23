// Ends a browser-project run whose page stopped running JavaScript.
//
// Test and hook timeouts are timers inside the test page, so a page whose
// main thread is blocked never fires them, and Vitest's own heartbeat is
// answered by the browser's network layer while the page is stuck. Without
// this reporter such a run waits for the CI job's timeout and names nothing.
//
// The reporter sees only public reporter events. A browser test file is
// running from the moment its page starts collecting it (queued) until it
// ends; every event the page reports for it restarts its clock. A running
// file silent for the bound is stalled: the reporter names it and the bound,
// sets a failing exit code and calls `vitest.exit(true)`. `close()` waits for
// the stuck run, so the exit lands after `teardownTimeout`, through
// `process.exit()`, which also stops the Playwright browser.
import type {
  Reporter,
  ReportedHookContext,
  TestCase,
  TestModule,
  TestProject,
  TestSuite,
  Vitest,
} from 'vitest/node';

export interface StalledPageWatchdogOptions {
  /** Silence from a running browser file that counts as a stalled page. */
  boundMs: number;
  /** How often the reporter checks for silence. */
  checkEveryMs?: number;
  now?: () => number;
}

export class StalledPageWatchdog implements Reporter {
  private readonly boundMs: number;
  private readonly checkEveryMs: number;
  private readonly now: () => number;
  private vitest: Vitest | undefined;
  private timer: ReturnType<typeof setInterval> | undefined;
  private fired = false;
  /** Running browser files and the time of the last event each reported. */
  private readonly lastEventAt = new Map<TestModule, number>();

  constructor(options: StalledPageWatchdogOptions) {
    this.boundMs = options.boundMs;
    this.checkEveryMs = options.checkEveryMs ?? 5_000;
    this.now = options.now ?? Date.now;
  }

  onInit(vitest: Vitest): void {
    this.vitest = vitest;
    // A live page fails a test or hook at its timeout and reports it. A
    // timeout at or above the bound would let a slow live test look stalled.
    for (const project of vitest.projects) {
      if (!isBrowserProject(project)) continue;
      const { testTimeout, hookTimeout } = project.config;
      if (testTimeout >= this.boundMs || hookTimeout >= this.boundMs) {
        throw new Error(
          `stalled-page watchdog: project "${project.name}" has testTimeout ${testTimeout} ms and ` +
            `hookTimeout ${hookTimeout} ms; both must stay below the ${this.boundMs} ms stall bound`,
        );
      }
    }
  }

  onTestRunStart(): void {
    this.lastEventAt.clear();
    this.fired = false;
    clearInterval(this.timer);
    this.timer = setInterval(() => this.check(), this.checkEveryMs);
    // The run, not this timer, decides when the process may exit.
    this.timer.unref?.();
  }

  onTestRunEnd(): void {
    clearInterval(this.timer);
    this.timer = undefined;
    this.lastEventAt.clear();
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
    this.lastEventAt.delete(module);
  }

  onHookStart(hook: ReportedHookContext): void {
    this.heard(moduleOf(hook.entity));
  }

  onHookEnd(hook: ReportedHookContext): void {
    this.heard(moduleOf(hook.entity));
  }

  onTestSuiteReady(suite: TestSuite): void {
    this.heard(suite.module);
  }

  onTestSuiteResult(suite: TestSuite): void {
    this.heard(suite.module);
  }

  onTestCaseReady(test: TestCase): void {
    this.heard(test.module);
  }

  onTestCaseResult(test: TestCase): void {
    this.heard(test.module);
  }

  onTestCaseAnnotate(test: TestCase): void {
    this.heard(test.module);
  }

  onTestCaseArtifactRecord(test: TestCase): void {
    this.heard(test.module);
  }

  private heard(module: TestModule): void {
    if (this.fired || !isBrowserProject(module.project)) return;
    // An event that arrives after the file ended must not revive it.
    if (module.state() !== 'queued' && module.state() !== 'pending') return;
    this.lastEventAt.set(module, this.now());
  }

  private check(): void {
    if (this.fired || !this.vitest) return;
    const now = this.now();
    const stalled = [...this.lastEventAt].filter(([, at]) => now - at >= this.boundMs);
    if (stalled.length === 0) return;
    this.fired = true;
    clearInterval(this.timer);
    const lines = stalled.map(([module, at]) => `  ${module.moduleId} (silent for ${Math.round((now - at) / 1000)} s)`);
    this.vitest.logger.error(
      `\nstalled-page watchdog: these browser test files reported nothing for ${this.boundMs / 1000} s ` +
        'while running, so their page stopped running JavaScript (a live page fails any test or hook ' +
        `at its timeout and reports it):\n${lines.join('\n')}\nEnding the run with a failure.\n`,
    );
    process.exitCode = 1;
    void this.vitest.exit(true).catch((error: unknown) => {
      this.vitest?.logger.error('stalled-page watchdog: vitest.exit failed', error);
      process.exit(1);
    });
  }
}

function isBrowserProject(project: TestProject): boolean {
  return project.config.browser.enabled === true;
}

function moduleOf(entity: TestModule | TestSuite | TestCase): TestModule {
  return entity.type === 'module' ? entity : entity.module;
}
