import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { TestCase, TestModule, TestProject, Vitest } from 'vitest/node';
import { StalledPageWatchdog } from './stalledPageWatchdog';

const BOUND_MS = 90_000;
const CHECK_MS = 5_000;

function project(name: string, browser: boolean, timeouts = { testTimeout: 60_000, hookTimeout: 60_000 }): TestProject {
  return { name, config: { browser: { enabled: browser }, ...timeouts } } as unknown as TestProject;
}

/**
 * A fresh object per call, as Vitest hands the queued event and the later
 * events of one file different TestModule objects.
 */
function testModule(moduleId: string, owner: TestProject): TestModule {
  return { type: 'module', moduleId, project: owner } as unknown as TestModule;
}

function testCase(module: TestModule): TestCase {
  return { type: 'test', module } as unknown as TestCase;
}

describe('StalledPageWatchdog', () => {
  let exitCode: typeof process.exitCode;
  let errors: string[];
  let exit: ReturnType<typeof vi.fn>;
  let vitest: Vitest;
  const browser = project('browser', true);
  const unit = project('unit', false);

  beforeEach(() => {
    vi.useFakeTimers();
    exitCode = process.exitCode;
    errors = [];
    exit = vi.fn(async () => undefined);
    vitest = {
      projects: [browser, unit],
      logger: { error: (message: string) => errors.push(message) },
      exit,
    } as unknown as Vitest;
  });

  afterEach(() => {
    process.exitCode = exitCode;
    vi.useRealTimers();
  });

  function start(): StalledPageWatchdog {
    const watchdog = new StalledPageWatchdog({ boundMs: BOUND_MS, checkEveryMs: CHECK_MS });
    watchdog.onInit(vitest);
    watchdog.onTestRunStart();
    return watchdog;
  }

  it('ends the run naming a running browser file that is silent for the bound', () => {
    const watchdog = start();
    watchdog.onTestModuleQueued(testModule('/src/stuck.browser.test.ts', browser));

    vi.advanceTimersByTime(BOUND_MS - CHECK_MS);
    expect(exit).not.toHaveBeenCalled();
    vi.advanceTimersByTime(CHECK_MS);

    expect(exit).toHaveBeenCalledExactlyOnceWith(true);
    expect(process.exitCode).toBe(1);
    const report = errors.join('\n');
    expect(report).toContain('/src/stuck.browser.test.ts (no report for 90 s)');
    expect(report).toContain('reported nothing for at least 90 s');
    expect(report).toContain('This usually means');
    vi.advanceTimersByTime(BOUND_MS * 2);
    expect(exit).toHaveBeenCalledTimes(1);
  });

  it('restarts a file clock on every event the page reports', () => {
    const watchdog = start();
    const id = '/src/live.browser.test.ts';
    watchdog.onTestModuleQueued(testModule(id, browser));
    for (let step = 0; step < 4; step += 1) {
      vi.advanceTimersByTime(BOUND_MS - CHECK_MS);
      watchdog.onTestCaseResult(testCase(testModule(id, browser)));
    }
    expect(exit).not.toHaveBeenCalled();
  });

  it('ends a file whose queued and later events carry different module objects', () => {
    const watchdog = start();
    const id = '/src/done.browser.test.ts';
    watchdog.onTestModuleQueued(testModule(id, browser));
    watchdog.onTestModuleCollected(testModule(id, browser));
    watchdog.onTestModuleEnd(testModule(id, browser));
    // Another file keeps the run reporting past the bound.
    const other = '/src/other.browser.test.ts';
    watchdog.onTestModuleQueued(testModule(other, browser));
    for (let step = 0; step < 4; step += 1) {
      vi.advanceTimersByTime(BOUND_MS - CHECK_MS);
      watchdog.onTestCaseResult(testCase(testModule(other, browser)));
    }
    expect(exit).not.toHaveBeenCalled();
  });

  it('does not revive a file from an event after its end', () => {
    const watchdog = start();
    const id = '/src/done.browser.test.ts';
    watchdog.onTestModuleQueued(testModule(id, browser));
    watchdog.onTestModuleEnd(testModule(id, browser));
    watchdog.onTestCaseResult(testCase(testModule(id, browser)));
    // A second file still running keeps the run-level rule out of it.
    const other = '/src/other.browser.test.ts';
    watchdog.onTestModuleQueued(testModule(other, browser));
    vi.advanceTimersByTime(BOUND_MS);
    expect(errors.join('\n')).not.toContain(id);
  });

  it('ends a run that reports nothing with no file running after a browser file', () => {
    const watchdog = start();
    const id = '/src/done.browser.test.ts';
    watchdog.onTestModuleQueued(testModule(id, browser));
    watchdog.onTestModuleEnd(testModule(id, browser));
    vi.advanceTimersByTime(BOUND_MS - CHECK_MS);
    watchdog.onUserConsoleLog();
    vi.advanceTimersByTime(BOUND_MS - CHECK_MS);
    expect(exit).not.toHaveBeenCalled();
    vi.advanceTimersByTime(CHECK_MS);
    expect(exit).toHaveBeenCalledExactlyOnceWith(true);
    expect(errors.join('\n')).toContain('with no test file running, and has not ended');
  });

  it('leaves a run alone that ran no browser file', () => {
    const watchdog = start();
    const id = '/src/slow.test.ts';
    watchdog.onTestModuleQueued(testModule(id, unit));
    vi.advanceTimersByTime(BOUND_MS * 2);
    watchdog.onTestModuleEnd(testModule(id, unit));
    vi.advanceTimersByTime(BOUND_MS * 2);
    expect(exit).not.toHaveBeenCalled();
  });

  it('stops checking when the run ends', () => {
    const watchdog = start();
    watchdog.onTestModuleQueued(testModule('/src/left.browser.test.ts', browser));
    watchdog.onTestRunEnd();
    vi.advanceTimersByTime(BOUND_MS * 2);
    expect(exit).not.toHaveBeenCalled();
  });

  it.each([
    { testTimeout: 60_000, hookTimeout: BOUND_MS },
    { testTimeout: 0, hookTimeout: 60_000 },
    { testTimeout: 60_000, hookTimeout: 0 },
  ])('refuses a browser project with testTimeout $testTimeout and hookTimeout $hookTimeout', (timeouts) => {
    vitest = { ...vitest, projects: [project('browser', true, timeouts)] } as Vitest;
    const watchdog = new StalledPageWatchdog({ boundMs: BOUND_MS });
    expect(() => watchdog.onInit(vitest)).toThrow(/project "browser".*set \(not 0\) and below the 90000 ms stall bound/);
  });
});
