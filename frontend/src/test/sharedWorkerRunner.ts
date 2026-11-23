// The test runner of the `unit` project (vitest.config.ts), whose workers
// run many files in one process against one happy-dom window. It opens
// each file as its collection starts and retires it (sharedWorker.ts) in
// `onAfterRunFiles`, the one point Vitest calls after every file's last
// hook whatever the file's mode: one that ran, one whose tests are all
// skipped or todo (no `afterAll` runs there), and one that failed while
// loading. The worker runs one file per `startTests`, so that point is
// per file. A worker of a run with more than one worker gets a new runner
// for each file, so the lifecycle is the worker's, not a runner's
// (WorkerLifecycle). A Vitest upgrade that stops calling that point for a
// skipped file or a file that fails while loading fails
// sharedWorkerRunner.test.ts.
//
// A file that leaves something behind fails, named, in the reporter and
// the exit code: retirement errors are added to the file's result and the
// result is sent again. So does the file that opens after something was
// installed while no file was open.
//
// Limit: work a file leaves running that is neither a dynamic import nor
// a tracked listener or timer (a promise a Node API resolves later, a
// MessageChannel, a library's own lazy import) can still reach the next
// file. A listener or timer such work installs after the file retired and
// before the next one opens is released and fails the next file; one it
// installs later is the next file's.
import { setTimeout as sleep } from 'node:timers/promises';
import { TestRunner, vi, type RunnerTestFile, type VitestTestRunner } from 'vitest';
import { SHARED_WORKER_KEY, TRACK_IMPORT_KEY } from './config/sharedWorkerKeys';
import { RESET_REGISTRY_KEY } from './config/testResets';
import type { ResetRegistry } from './resetRegistry';
import { sharedGlobalChanges, snapshotSharedGlobals, type SharedGlobalsSnapshot } from './sharedGlobals';
import { FileImports, FileResources, retireFile } from './sharedWorker';

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

// The worker's one file lifecycle, a worker global that every runner of
// the worker uses: one set of listener and timer wrappers, one open file.
class WorkerLifecycle {
  private readonly resources: FileResources;
  private readonly imports = new FileImports();
  private file: RunnerTestFile | null = null;
  // What was installed while no file was open, charged to this file.
  private opening: Error | null = null;
  private start: { readonly globals: SharedGlobalsSnapshot; readonly href: string } | null = null;

  // Built at the first collection: the environment's window exists, and no
  // setup file has loaded an app module yet.
  constructor() {
    const resources = new FileResources({
      setTimeout: globalThis.setTimeout,
      clearTimeout: globalThis.clearTimeout,
      setInterval: globalThis.setInterval,
      clearInterval: globalThis.clearInterval,
      setImmediate: globalThis.setImmediate,
      clearImmediate: globalThis.clearImmediate,
    });
    resources.trackListeners(window);
    resources.trackListeners(document);
    // happy-dom keeps a window resize listener for each MediaQueryList
    // change listener, out of reach of the window wrapper above.
    const matchMedia = window.matchMedia;
    window.matchMedia = function (this: Window, query: string): MediaQueryList {
      const list = matchMedia.call(this, query);
      resources.trackListeners(list);
      return list;
    };
    resources.trackTimers(globalThis);
    (globalThis as { [key: symbol]: unknown })[Symbol.for(TRACK_IMPORT_KEY)] = <T>(promise: Promise<T>, label: string) => this.imports.track(promise, label);
    this.resources = resources;
  }

  open(file: RunnerTestFile): void {
    this.file = file;
    this.start = null;
    this.imports.openFile(file.name);
    this.opening = this.resources.openFile(file.name);
  }

  /** Once setup.ts has run, before the test file is imported, so the test
   * file's module-scope changes count against it (sharedWorkerStart.ts). */
  startFile(): void {
    this.start = { globals: snapshotSharedGlobals(), href: location.href };
  }

  /** Retires the open file and returns it with what it did wrong. */
  async retire(): Promise<{ readonly file: RunnerTestFile; readonly errors: Error[] } | null> {
    const { file, start, resources, opening } = this;
    if (file === null) return null;
    this.file = null;
    this.opening = null;
    const registry = () => (globalThis as { [key: symbol]: ResetRegistry | undefined })[Symbol.for(RESET_REGISTRY_KEY)];
    const errors = await retireFile(file.name, {
      imports: this.imports,
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
      releaseResources: () => resources.releaseFile(),
      resetDocument: () => resetDocument(start?.href ?? null),
      resetModules: () => vi.resetModules(),
      globalChanges: () => (start === null ? [] : sharedGlobalChanges(start.globals)),
      importBoundMs: IMPORT_BOUND_MS,
      sleep: (ms) => sleep(ms),
      inFlight: () => this.imports.inFlight(),
    });
    if (opening !== null) errors.unshift(opening);
    // A file that failed before its setup finished has failed already.
    if (start === null && file.result?.state !== 'fail') {
      errors.push(new Error(`sharedWorkerStart.ts did not run for ${file.name}: it must be the last setup file of the unit project`));
    }
    return { file, errors };
  }
}

/** The worker's lifecycle, built by the first runner that asks. */
function workerLifecycle(): WorkerLifecycle {
  const worker = globalThis as { [key: symbol]: WorkerLifecycle | undefined };
  const key = Symbol.for(SHARED_WORKER_KEY);
  return (worker[key] ??= new WorkerLifecycle());
}

export default class SharedWorkerRunner extends TestRunner {
  // Vitest installs it on every runner, to send task updates to the run.
  declare onTaskUpdate?: VitestTestRunner['onTaskUpdate'];

  override onCollectStart(file: RunnerTestFile): void {
    super.onCollectStart(file);
    workerLifecycle().open(file);
  }

  override async onAfterRunFiles(files: RunnerTestFile[]): Promise<void> {
    const retired = await workerLifecycle().retire();
    if (retired !== null && retired.errors.length > 0) await this.fail(retired.file, retired.errors);
    super.onAfterRunFiles(files);
  }

  private async fail(file: RunnerTestFile, errors: Error[]): Promise<void> {
    const result = file.result ?? { state: 'fail' as const };
    result.state = 'fail';
    result.errors = [...(result.errors ?? []), ...errors.map((error) => ({ name: error.name, message: error.message, stack: error.stack }))];
    file.result = result;
    // The file's result was sent when its suite finished; send it again.
    await this.onTaskUpdate?.([[file.id, result, file.meta]], []);
  }
}
