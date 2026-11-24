// The file lifecycle of the `unit` project, whose workers run many files in
// one process against one happy-dom window (vitest.config.ts). The
// worker's one lifecycle (sharedWorkerRunner.ts) opens a file as the
// file's collection starts and retires it, with `retireFile`, once the
// file's last hook has run, whatever the file's mode.
//
// Each open file owns one `OpenFile`: the dynamic imports, listeners and
// timers its code started. A file stays open until its retirement has run
// every step that can run its code, so all of that is its own. Retiring a
// file leaves the worker as a fresh one would find it:
//   - the dynamic imports the file started have settled, so none of its
//     code evaluates later (the bound below fails the file otherwise), and
//     one that rejects while the file retires fails the file;
//   - Vitest's stubs, mocks and fake timers are restored, and the document
//     body, focus, selection, URL and cookies are reset; the imports that
//     teardown starts settle too, until a pass starts none;
//   - the listeners its code added to `window`, `document` or a media
//     query list, and the timers it left pending, are released;
//   - app modules are re-evaluated for the next file (`vi.resetModules`);
//   - a file that replaces a global, a DOM prototype member or a host
//     object property must put it back itself; sharedGlobals.ts fails the
//     file that leaves one changed.
//
// Work that outlives a retired file, which none of the above tracks (a
// Node API's callback, a library's own timer), can still run later. A
// listener, timer or import it installs while no file is open is released
// at once and fails the next file. After the worker's last file there is
// no next file, and no test left for it to reach.
import { fileURLToPath } from 'node:url';

/** What retiring a file needs besides the file; the runner passes the
 * worker's own. */
export type Retirement = {
  /** Binding calls with no mock since the last check (resetRegistry.ts). */
  takeUnansweredBindings(): string[];
  /** Vitest's own per-file state: stubs, mocks, fake timers. */
  restoreVitest(): void;
  resetDocument(): Promise<void>;
  resetModules(): void;
  /** The globals the file left changed. */
  globalChanges(): string[];
  /** How long the file's imports may take to settle, in all. */
  readonly importBoundMs: number;
  /** Resolves after `ms` on a timer no file owns; `signal` cancels it. */
  sleep(ms: number, signal: AbortSignal): Promise<void>;
};

const BOUND = Symbol('bound');

/** Retires `file` and returns what it did wrong; the caller fails the file
 * with them. The file stays open, so what its imports and its teardown
 * install is its own, until nothing it started is left to run. */
export async function retireFile(file: OpenFile, files: WorkerFiles, retirement: Retirement): Promise<Error[]> {
  const errors: Error[] = [];
  const cancel = new AbortController();
  let bound: Promise<typeof BOUND | null> | null = null;
  // Created when the file first has an import in flight, so a file with
  // none starts no timer, and cancelled once the file is retired.
  const startBound = () =>
    (bound ??= retirement.sleep(retirement.importBoundMs, cancel.signal).then(
      () => BOUND,
      (error: unknown) => {
        if (cancel.signal.aborted) return null;
        throw error;
      },
    ));
  let gaveUp = false;
  try {
    do {
      if (!(await file.drain(startBound))) gaveUp = true;
      // Stubs and spies taken over fake timers come off before the timers do.
      retirement.restoreVitest();
      await retirement.resetDocument();
    } while (!gaveUp && file.hasUndrained());
  } finally {
    cancel.abort();
  }
  if (gaveUp) {
    errors.push(new Error(
      `${file.name} left dynamic imports in flight ${retirement.importBoundMs}ms after its last test: ${file.inFlight().join('; ')}. ` +
      'Await the import, or the condition that waits for it, in the test that starts it.',
    ));
  }
  for (const rejection of file.takeRejections()) {
    errors.push(new Error(
      `${file.name} started a dynamic import that rejected after its last test: ${rejection}. Await the import in the test that starts it.`,
    ));
  }
  const calls = retirement.takeUnansweredBindings();
  if (calls.length > 0) {
    errors.push(new Error(
      `${calls.length} binding call${calls.length === 1 ? '' : 's'} had no mock installed:\n  ${calls.join('\n  ')}\n` +
      'Install an answer with setBindingMock in the test.',
    ));
  }
  files.close(file);
  retirement.resetModules();
  const changes = retirement.globalChanges();
  if (changes.length > 0) {
    errors.push(new Error(
      `${file.name} left shared globals changed for the next file in its worker; restore them in afterEach or afterAll, or use vi.stubGlobal: ${changes.join('; ')}`,
    ));
  }
  return errors;
}

const THIS_FILE = fileURLToPath(import.meta.url);

type Timer = ReturnType<typeof setTimeout> | ReturnType<typeof setImmediate>;
type TimerKind = 'timeout' | 'interval' | 'immediate';

type RealTimers = {
  readonly setTimeout: typeof setTimeout;
  readonly clearTimeout: typeof clearTimeout;
  readonly setInterval: typeof setInterval;
  readonly clearInterval: typeof clearInterval;
  readonly setImmediate: typeof setImmediate;
  readonly clearImmediate: typeof clearImmediate;
};

type ListenerEntry = { readonly target: EventTarget; readonly type: string; readonly capture: boolean };

type StartedImport = {
  readonly promise: Promise<unknown>;
  readonly label: string;
  /** Started once the file's retirement had begun. */
  readonly late: boolean;
};

function captureOf(options: boolean | EventListenerOptions | undefined): boolean {
  return typeof options === 'boolean' ? options : options?.capture === true;
}

function describe(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** One file's state: what its code installed and imported while it was
 * open. A retirement holds only its own file's. */
export class OpenFile {
  readonly listeners = new Map<EventListenerOrEventListenerObject, ListenerEntry[]>();
  readonly timers = new Map<Timer, TimerKind>();
  private started: StartedImport[] = [];
  private readonly pending = new Set<StartedImport>();
  private readonly rejections: string[] = [];
  private retiring = false;

  constructor(readonly name: string) {}

  track(promise: Promise<unknown>, label: string): void {
    this.started.push({ promise, label, late: this.retiring });
  }

  hasUndrained(): boolean {
    return this.started.length > 0;
  }

  /** Waits for every import the file started and the imports those start,
   * or for the bound `startBound` starts; false when the bound came first.
   *
   * No handler is attached to an import until here, so one that rejects
   * while the file's tests run reaches its caller, and is reported unhandled
   * if no caller handles it, exactly as without tracking. Attaching one
   * marks the import handled, and nothing tells whether another handler
   * exists, so an import that rejects from here on fails the file. One that
   * had already rejected was reported then. */
  async drain(startBound: () => Promise<typeof BOUND | null>): Promise<boolean> {
    this.retiring = true;
    while (this.started.length > 0) {
      const batch = this.started.splice(0);
      // A settled promise's handlers are queued as they are attached, ahead
      // of the job queued after them: what settles after that job settled
      // during retirement.
      let attaching = true;
      const settled = Promise.all(batch.map((entry) => {
        this.pending.add(entry);
        return entry.promise.then(
          () => {
            this.pending.delete(entry);
          },
          (error: unknown) => {
            this.pending.delete(entry);
            if (!attaching || entry.late) this.rejections.push(`${entry.label}: ${describe(error)}`);
          },
        );
      }));
      await new Promise<void>((attached) => queueMicrotask(() => {
        attaching = false;
        attached();
      }));
      if (this.pending.size > 0 && (await Promise.race([settled, startBound()])) === BOUND) return false;
      await settled;
    }
    return true;
  }

  /** The imports still in flight, for the bound's message. */
  inFlight(): string[] {
    return [...this.pending].map(({ label }) => label);
  }

  takeRejections(): string[] {
    return this.rejections.splice(0);
  }
}

/** The worker's wrappers around listener methods, timer functions and
 * dynamic imports, and the one file open, which each call is charged to. */
export class WorkerFiles {
  private current: OpenFile | null = null;
  private retired = 'no file';
  private leftovers: string[] = [];
  private readonly real: RealTimers;

  constructor(real: RealTimers) {
    this.real = real;
  }

  /** Opens `file`, with the error to fail it with when something was
   * installed while no file was open: that was released at once, and this
   * is the next file. */
  open(name: string): { readonly file: OpenFile; readonly leftovers: Error | null } {
    const file = new OpenFile(name);
    this.current = file;
    const leftovers = this.leftovers;
    this.leftovers = [];
    if (leftovers.length === 0) return { file, leftovers: null };
    return {
      file,
      leftovers: new Error(
        `after ${this.retired} retired and before ${name} started, code installed ${leftovers.length === 1 ? 'a resource' : `${leftovers.length} resources`}, released at once:\n${leftovers.join('\n')}`,
      ),
    };
  }

  /** Releases `file`'s listeners and timers and closes it. */
  close(file: OpenFile): void {
    if (this.current === file) this.current = null;
    this.retired = file.name;
    for (const [handle, kind] of file.timers) this.clear(handle, kind);
    file.timers.clear();
    for (const [listener, entries] of file.listeners) {
      for (const { target, type, capture } of entries) target.removeEventListener(type, listener, capture);
    }
    file.listeners.clear();
  }

  /** Charges a dynamic import to the open file; hands back the same
   * promise. */
  track<T>(promise: Promise<T>, label: string): Promise<T> {
    if (this.current !== null) this.current.track(promise, label);
    else this.leftover(`a dynamic import, ${label}`);
    return promise;
  }

  private clear(handle: Timer, kind: TimerKind): void {
    if (kind === 'timeout') this.real.clearTimeout(handle as ReturnType<typeof setTimeout>);
    else if (kind === 'interval') this.real.clearInterval(handle as ReturnType<typeof setInterval>);
    else this.real.clearImmediate(handle as ReturnType<typeof setImmediate>);
  }

  /** Records `what`, installed while no file is open, for the next file.
   * Vitest's own timers are not wrapped: it took them before this module
   * loaded. */
  private leftover(what: string): void {
    // The installing code's frames, without this module's wrappers.
    const frames = (new Error().stack ?? '').split('\n').slice(1).filter((frame) => !frame.includes(`${THIS_FILE}:`));
    const where = frames.length > 0 ? frames.slice(0, 5).join('\n') : '    (no stack)';
    this.leftovers.push(`  ${what}, installed at\n${where}`);
  }

  /** The file the timer is charged to, or null when it was released. */
  private addTimer(handle: Timer, kind: TimerKind): OpenFile | null {
    if (this.current !== null) {
      this.current.timers.set(handle, kind);
      return this.current;
    }
    this.clear(handle, kind);
    this.leftover(`a pending ${kind}`);
    return null;
  }

  private deleteTimer(handle: unknown): void {
    this.current?.timers.delete(handle as Timer);
  }

  private listenerAdded(target: EventTarget, type: string, listener: EventListenerOrEventListenerObject, options: boolean | AddEventListenerOptions | undefined, remove: EventTarget['removeEventListener']): void {
    const file = this.current;
    if (file === null) {
      remove.call(target, type, listener, options);
      this.leftover(`a '${type}' listener`);
      return;
    }
    const entries = file.listeners.get(listener) ?? [];
    entries.push({ target, type, capture: captureOf(options) });
    file.listeners.set(listener, entries);
  }

  private listenerRemoved(target: EventTarget, type: string, listener: EventListenerOrEventListenerObject, options: boolean | EventListenerOptions | undefined): void {
    const file = this.current;
    const entries = file?.listeners.get(listener);
    if (file === null || entries === undefined) return;
    const capture = captureOf(options);
    const index = entries.findIndex((entry) => entry.target === target && entry.type === type && entry.capture === capture);
    if (index >= 0) entries.splice(index, 1);
    if (entries.length === 0) file.listeners.delete(listener);
  }

  /** Wraps `target`'s listener methods. */
  trackListeners(target: EventTarget): void {
    const add = target.addEventListener;
    const remove = target.removeEventListener;
    const added = this.listenerAdded.bind(this);
    const removed = this.listenerRemoved.bind(this);
    Object.defineProperty(target, 'addEventListener', {
      configurable: true,
      writable: true,
      value: function addEventListener(this: EventTarget, type: string, listener: EventListenerOrEventListenerObject | null, options?: boolean | AddEventListenerOptions): void {
        add.call(this, type, listener, options);
        if (listener !== null) added(this, type, listener, options, remove);
      },
    });
    Object.defineProperty(target, 'removeEventListener', {
      configurable: true,
      writable: true,
      value: function removeEventListener(this: EventTarget, type: string, listener: EventListenerOrEventListenerObject | null, options?: boolean | EventListenerOptions): void {
        remove.call(this, type, listener, options);
        if (listener !== null) removed(this, type, listener, options);
      },
    });
  }

  /** Wraps `host`'s timer functions. */
  trackTimers(host: typeof globalThis): void {
    const { real } = this;
    const added = this.addTimer.bind(this);
    const deleted = this.deleteTimer.bind(this);
    host.setTimeout = function setTimeout(callback: unknown, delay?: number, ...args: unknown[]) {
      if (typeof callback !== 'function') return real.setTimeout(callback as () => void, delay);
      let owner: OpenFile | null = null;
      const handle = real.setTimeout(() => {
        owner?.timers.delete(handle);
        callback(...args);
      }, delay);
      owner = added(handle, 'timeout');
      return handle;
    } as typeof globalThis.setTimeout;
    host.setInterval = function setInterval(callback: unknown, delay?: number, ...args: unknown[]) {
      const handle = real.setInterval(callback as (...a: unknown[]) => void, delay, ...args);
      added(handle, 'interval');
      return handle;
    } as typeof globalThis.setInterval;
    host.setImmediate = function setImmediate(callback: (...a: unknown[]) => void, ...args: unknown[]) {
      let owner: OpenFile | null = null;
      const handle = real.setImmediate(() => {
        owner?.timers.delete(handle);
        callback(...args);
      });
      owner = added(handle, 'immediate');
      return handle;
    } as typeof globalThis.setImmediate;
    host.clearTimeout = function clearTimeout(handle) {
      deleted(handle);
      real.clearTimeout(handle);
    } as typeof globalThis.clearTimeout;
    host.clearInterval = function clearInterval(handle) {
      deleted(handle);
      real.clearInterval(handle);
    } as typeof globalThis.clearInterval;
    host.clearImmediate = function clearImmediate(handle) {
      deleted(handle);
      real.clearImmediate(handle);
    } as typeof globalThis.clearImmediate;
  }
}
