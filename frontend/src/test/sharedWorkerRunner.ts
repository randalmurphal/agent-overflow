// The test runner of the `unit` project (vitest.config.ts), whose workers
// run many files in one process against one happy-dom window. It opens
// each file just before Vitest collects it (`onBeforeCollect`) and retires
// it (sharedWorker.ts) before Vitest publishes the file's result, so a retirement failure is the
// file's first result, in the reporter and the exit code:
//   - a file that runs, in `onAfterRunSuite` for the file, after its last
//     hook and fixture teardown;
//   - a file whose tests are all skipped or todo, in `onBeforeRunSuite` for
//     the file: none of its code runs after that;
//   - a file that failed while loading, or was collected without a run
//     (`vitest list`, an editor's test explorer), in `onBeforeCollect` of
//     the worker's next file. Its result, already published, is sent again
//     with the retirement's errors.
// The worker's last file of that last kind is never retired; no test is
// left for what it leaves to reach.
// A file that opens after something was installed while no file was open
// fails for it the same way. A worker of a run with more than one worker
// gets a new runner for each file, so the lifecycle is the worker's, not a
// runner's (WorkerLifecycle). sharedWorkerRunner.test.ts fails when a
// Vitest upgrade stops calling these points or stops publishing a result
// they change.
//
// Limit: work a file leaves running that is neither a dynamic import nor
// a tracked listener or timer (a promise a Node API resolves later, a
// MessageChannel, a library's own lazy import) can still reach the next
// file. A listener, timer or import such work installs after the file
// retired and before the next one opens is released and fails the next
// file; one it installs later is the next file's.
import { posix } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { TestRunner, vi, type RunnerTestFile, type RunnerTestSuite, type VitestTestRunner } from 'vitest';
import { SHARED_WORKER_KEY, TRACK_IMPORT_KEY } from './config/sharedWorkerKeys';
import { RESET_REGISTRY_KEY } from './config/testResets';
import type { ResetRegistry } from './resetRegistry';
import { sharedGlobalChanges, snapshotSharedGlobals, type SharedGlobalsSnapshot } from './sharedGlobals';
import { WorkerFiles, retireFile, type OpenFile } from './sharedWorker';

// How long a file's dynamic imports may take to settle after its last
// hook. A test that awaits what it starts never waits here.
const IMPORT_BOUND_MS = 10_000;

// happy-dom's control API on the environment's window.
type HappyDOM = { abort(): Promise<void>; setURL(url: string): void };

// A fresh window has an empty, unfocused body with no selection, no
// attributes on <html> or <body>, no cookies and the configured URL.
// <head> keeps the styles Svelte injects once per component module.
async function resetDocument(href: string | null): Promise<void> {
  await (window as unknown as { happyDOM: HappyDOM }).happyDOM.abort();
  (document.activeElement as HTMLElement | null)?.blur?.();
  document.getSelection()?.removeAllRanges();
  document.body.replaceChildren();
  for (const element of [document.documentElement, document.body]) {
    for (const name of element.getAttributeNames()) element.removeAttribute(name);
  }
  for (const cookie of document.cookie.split(';')) {
    const name = cookie.split('=')[0]?.trim();
    if (name) document.cookie = `${name}=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/`;
  }
  if (href !== null && location.href !== href) (window as unknown as { happyDOM: HappyDOM }).happyDOM.setURL(href);
}

type Start = { readonly globals: SharedGlobalsSnapshot; readonly href: string };

// The open file: its path, its task once collected, its state, what was
// installed before it opened, and what it starts from once setup.ts ran.
type Open = { readonly path: string; task: RunnerTestFile | null; readonly file: OpenFile; readonly leftovers: Error | null; start: Start | null };

// The worker's one file lifecycle, a worker global that every runner of
// the worker uses: one set of wrappers, one open file.
class WorkerLifecycle {
  private readonly files: WorkerFiles;
  private current: Open | null = null;

  // Built before the first collection: the environment's window exists,
  // and no setup file has loaded an app module yet.
  constructor() {
    const files = new WorkerFiles({
      setTimeout: globalThis.setTimeout,
      clearTimeout: globalThis.clearTimeout,
      setInterval: globalThis.setInterval,
      clearInterval: globalThis.clearInterval,
      setImmediate: globalThis.setImmediate,
      clearImmediate: globalThis.clearImmediate,
    });
    files.trackListeners(window);
    files.trackListeners(document);
    // happy-dom keeps a window resize listener for each MediaQueryList
    // change listener, out of reach of the window wrapper above.
    const matchMedia = window.matchMedia;
    window.matchMedia = function (this: Window, query: string): MediaQueryList {
      const list = matchMedia.call(this, query);
      files.trackListeners(list);
      return list;
    };
    files.trackTimers(globalThis);
    (globalThis as { [key: symbol]: unknown })[Symbol.for(TRACK_IMPORT_KEY)] = <T>(promise: Promise<T>, label: string) => files.track(promise, label);
    this.files = files;
  }

  /** Opens the file at `path`, named as Vitest names it (`name`). */
  open(path: string, name: string): void {
    this.current = { path, task: null, ...this.files.open(name), start: null };
  }

  /** The open file's task, once Vitest has collected it. */
  collected(task: RunnerTestFile): void {
    if (this.current?.path !== task.filepath) throw new Error(`Vitest collected ${task.filepath}, but ${this.current?.path ?? 'no file'} is open`);
    this.current.task = task;
  }

  /** Once setup.ts has run, before the test file is imported, so the test
   * file's module-scope changes count against it (sharedWorkerStart.ts). */
  startFile(): void {
    if (this.current === null) throw new Error('sharedWorkerStart.ts ran with no test file open');
    this.current.start = { globals: snapshotSharedGlobals(), href: location.href };
  }

  /** Retires the open file, when it is `task` or `task` is omitted, and
   * returns it with what it did wrong. */
  async retire(task?: RunnerTestFile): Promise<{ readonly task: RunnerTestFile | null; readonly name: string; readonly errors: Error[] } | null> {
    const open = this.current;
    if (open === null || (task !== undefined && open.task !== task)) return null;
    this.current = null;
    const { start } = open;
    const registry = () => (globalThis as { [key: symbol]: ResetRegistry | undefined })[Symbol.for(RESET_REGISTRY_KEY)];
    const errors = await retireFile(open.file, this.files, {
      takeUnansweredBindings: () => registry()?.takeUnansweredBindings() ?? [],
      restoreVitest: () => {
        vi.unstubAllGlobals();
        vi.unstubAllEnvs();
        vi.restoreAllMocks();
        // Vitest holds every mock that recorded a call, with its
        // arguments, until it is cleared.
        vi.clearAllMocks();
        vi.useRealTimers();
      },
      resetDocument: () => resetDocument(start?.href ?? null),
      resetModules: () => vi.resetModules(),
      globalChanges: () => (start === null ? [] : sharedGlobalChanges(start.globals)),
      importBoundMs: IMPORT_BOUND_MS,
      sleep: (ms, signal) => sleep(ms, undefined, { signal }),
    });
    if (open.leftovers !== null) errors.unshift(open.leftovers);
    // A file that failed before its setup finished has failed already.
    if (start === null && open.task?.result?.state !== 'fail') {
      errors.push(new Error(`sharedWorkerStart.ts did not run for ${open.file.name}: it must be the last setup file of the unit project`));
    }
    return { task: open.task, name: open.file.name, errors };
  }
}

/** The worker's lifecycle, built by the first runner that asks. */
function workerLifecycle(): WorkerLifecycle {
  const worker = globalThis as { [key: symbol]: WorkerLifecycle | undefined };
  const key = Symbol.for(SHARED_WORKER_KEY);
  return (worker[key] ??= new WorkerLifecycle());
}

function isFile(suite: RunnerTestSuite): suite is RunnerTestFile {
  return 'filepath' in suite;
}

/** Adds retirement errors to the file's result, making it a failure. */
function failFile(task: RunnerTestFile, errors: Error[]): void {
  const result = task.result ?? { state: 'fail' as const };
  result.state = 'fail';
  result.errors = [...(result.errors ?? []), ...errors.map((error) => ({ name: error.name, message: error.message, stack: error.stack }))];
  task.result = result;
}

export default class SharedWorkerRunner extends TestRunner {
  // Vitest installs it on every runner, to send task updates to the run.
  declare onTaskUpdate?: VitestTestRunner['onTaskUpdate'];

  /** Opens the file about to be collected: Vitest awaits this, before it
   * creates the file's task (it does not await `onCollectStart`). A file
   * still open failed while loading or was collected without a run, and its
   * result went out already: it retires first, and its result is sent
   * again with the retirement's errors. */
  async onBeforeCollect(paths: string[]): Promise<void> {
    const lifecycle = workerLifecycle();
    const retired = await lifecycle.retire();
    if (retired !== null && retired.errors.length > 0) {
      // A file whose tags were refused has no task.
      if (retired.task === null) throw new AggregateError(retired.errors, `${retired.name} left the worker changed`);
      failFile(retired.task, retired.errors);
      await this.onTaskUpdate?.([[retired.task.id, retired.task.result, retired.task.meta]], []);
    }
    if (paths.length !== 1) throw new Error(`the unit project's runner opens one file per collection; Vitest collected ${paths.length} together`);
    lifecycle.open(paths[0]!, posix.relative(this.config.root, paths[0]!));
  }

  async onCollected(files: RunnerTestFile[]): Promise<void> {
    for (const file of files) workerLifecycle().collected(file);
  }

  /** A file whose tests are all skipped or todo retires here: none of its
   * code runs after this, and a failed result makes Vitest report it as a
   * file that failed early. Vitest counts it skipped as well, from the mode
   * it collected it in. */
  override async onBeforeRunSuite(suite: RunnerTestSuite): Promise<void> {
    await super.onBeforeRunSuite(suite);
    if (!isFile(suite) || (suite.mode !== 'skip' && suite.mode !== 'todo')) return;
    const retired = await workerLifecycle().retire(suite);
    if (retired !== null && retired.errors.length > 0) failFile(suite, retired.errors);
  }

  /** A file that ran retires after its last hook and fixture teardown, and
   * before Vitest publishes its result, so the first result is final. */
  override async onAfterRunSuite(suite: RunnerTestSuite): Promise<void> {
    await super.onAfterRunSuite(suite);
    if (!isFile(suite)) return;
    const retired = await workerLifecycle().retire(suite);
    if (retired !== null && retired.errors.length > 0) failFile(suite, retired.errors);
  }
}
