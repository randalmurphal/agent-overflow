import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { TestCase, TestModule, TestProject, Vitest } from 'vitest/node';
import { StalledPageWatchdog } from './stalledPageWatchdog';

const BOUND_MS = 90_000;
const CHECK_MS = 5_000;

function project(name: string, browser: boolean, timeouts = { testTimeout: 60_000, hookTimeout: 60_000 }): TestProject {
  return { name, config: { browser: { enabled: browser }, ...timeouts } } as unknown as TestProject;
}

function testModule(moduleId: string, owner: TestProject): TestModule & { setState(s: string): void } {
  let state = 'queued';
  return {
    type: 'module',
    moduleId,
    project: owner,
    state: () => state,
    setState(next: string) {
      state = next;
    },
  } as unknown as TestModule & { setState(s: string): void };
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
    const stuck = testModule('/src/stuck.browser.test.ts', browser);
    watchdog.onTestModuleQueued(stuck);

    vi.advanceTimersByTime(BOUND_MS - CHECK_MS);
    expect(exit).not.toHaveBeenCalled();
    vi.advanceTimersByTime(CHECK_MS);

    expect(exit).toHaveBeenCalledExactlyOnceWith(true);
    expect(process.exitCode).toBe(1);
    expect(errors.join('\n')).toContain('/src/stuck.browser.test.ts');
    expect(errors.join('\n')).toContain('reported nothing for 90 s');
    vi.advanceTimersByTime(BOUND_MS * 2);
    expect(exit).toHaveBeenCalledTimes(1);
  });

  it('restarts a file clock on every event the page reports', () => {
    const watchdog = start();
    const live = testModule('/src/live.browser.test.ts', browser);
    watchdog.onTestModuleQueued(live);
    live.setState('pending');
    for (let step = 0; step < 4; step += 1) {
      vi.advanceTimersByTime(BOUND_MS - CHECK_MS);
      watchdog.onTestCaseResult(testCase(live));
    }
    expect(exit).not.toHaveBeenCalled();
  });

  it('forgets a file once it ends, even if a late event arrives', () => {
    const watchdog = start();
    const done = testModule('/src/done.browser.test.ts', browser);
    watchdog.onTestModuleQueued(done);
    done.setState('passed');
    watchdog.onTestModuleEnd(done);
    watchdog.onTestCaseResult(testCase(done));
    vi.advanceTimersByTime(BOUND_MS * 2);
    expect(exit).not.toHaveBeenCalled();
  });

  it('ignores files outside browser projects', () => {
    const watchdog = start();
    watchdog.onTestModuleQueued(testModule('/src/slow.test.ts', unit));
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

  it('refuses a browser project whose own timeouts reach the bound', () => {
    vitest = { ...vitest, projects: [project('browser', true, { testTimeout: 60_000, hookTimeout: BOUND_MS })] } as Vitest;
    const watchdog = new StalledPageWatchdog({ boundMs: BOUND_MS });
    expect(() => watchdog.onInit(vitest)).toThrow(/project "browser".*below the 90000 ms stall bound/);
  });
});
