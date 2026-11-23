// The file lifecycle of the `unit` project, whose workers run many files in
// one process against one happy-dom window (vitest.config.ts). The
// worker's one lifecycle (sharedWorkerRunner.ts) opens a file as the
// file's collection starts and retires it, with `retireFile`, once the
// file's last hook has run, whatever the file's mode.
//
// Retiring a file leaves the worker as a fresh one would find it:
//   - the dynamic imports the file started have settled, so none of its
//     code evaluates later (the bound below fails the file otherwise);
//   - the listeners its code added to `window`, `document` or a media
//     query list, and the timers it left pending, are released;
//   - the document body, focus, selection, URL and cookies are reset;
//   - app modules are re-evaluated for the next file (`vi.resetModules`);
//   - a file that replaces a global, a DOM prototype member or a host
//     object property must put it back itself; sharedGlobals.ts fails the
//     file that leaves one changed.
//
// A listener or timer installed while no file is open (after one file
// retired, before the next opened) is released at once, and the next file
// fails naming it: no file is open to charge.

/** What retiring a file needs; the runner passes the worker's own. */
export type Retirement = {
  /** The file's dynamic imports: resolves once each has settled. */
  readonly imports: { settled(): Promise<void> };
  /** Binding calls with no mock since the last check (resetRegistry.ts). */
  takeUnansweredBindings(): string[];
  /** Vitest's own per-file state: stubs, mocks, fake timers. */
  restoreVitest(): void;
  /** Releases the file's listeners and timers. */
  releaseResources(): void;
  resetDocument(): Promise<void>;
  resetModules(): void;
  /** The globals the file left changed. */
  globalChanges(): string[];
  /** How long the file's imports may take to settle. */
  readonly importBoundMs: number;
  /** Resolves after `ms` on a timer no file owns. */
  sleep(ms: number): Promise<void>;
  /** The imports still in flight, for the bound's message. */
  inFlight(): string[];
};

/** Retires one file and returns what it did wrong; the caller fails the
 * file with them. Order matters: the file's imports settle before anything
 * is released, so what they install is released too. */
export async function retireFile(file: string, retirement: Retirement): Promise<Error[]> {
  const errors: Error[] = [];
  const bound = Symbol('bound');
  const settled = await Promise.race([retirement.imports.settled(), retirement.sleep(retirement.importBoundMs).then(() => bound)]);
  if (settled === bound) {
    errors.push(new Error(
      `${file} left dynamic imports in flight ${retirement.importBoundMs}ms after its last test: ${retirement.inFlight().join('; ')}. ` +
      'Await the import, or the condition that waits for it, in the test that starts it.',
    ));
  }
  const calls = retirement.takeUnansweredBindings();
  if (calls.length > 0) {
    errors.push(new Error(
      `${calls.length} binding call${calls.length === 1 ? '' : 's'} had no mock installed:\n  ${calls.join('\n  ')}\n` +
      'Install an answer with setBindingMock in the test.',
    ));
  }
  // Stubs and spies taken over fake timers come off before the timers do.
  retirement.restoreVitest();
  retirement.releaseResources();
  await retirement.resetDocument();
  retirement.resetModules();
  const changes = retirement.globalChanges();
  if (changes.length > 0) {
    errors.push(new Error(
      `${file} left shared globals changed for the next file in its worker; restore them in afterEach or afterAll, or use vi.stubGlobal: ${changes.join('; ')}`,
    ));
  }
  return errors;
}

const THIS_FILE = new URL(import.meta.url).pathname;

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

type OpenFile = {
  readonly file: string;
  readonly listeners: Map<EventListenerOrEventListenerObject, ListenerEntry[]>;
  readonly timers: Map<Timer, TimerKind>;
};

function captureOf(options: boolean | EventListenerOptions | undefined): boolean {
  return typeof options === 'boolean' ? options : options?.capture === true;
}

/** The listeners and timers each file's code installs. */
export class FileResources {
  private open: OpenFile | null = null;
  private retired = 'no file';
  private leftovers: string[] = [];
  private readonly real: RealTimers;

  constructor(real: RealTimers) {
    this.real = real;
  }

  /** Starts collecting into `file`. Returns the error to fail `file`
   * with when something was installed while no file was open: it was
   * released at once, and this is the next file. */
  openFile(file: string): Error | null {
    this.open = { file, listeners: new Map(), timers: new Map() };
    const leftovers = this.leftovers;
    this.leftovers = [];
    if (leftovers.length === 0) return null;
    return new Error(
      `after ${this.retired} retired and before ${file} started, code installed ${leftovers.length === 1 ? 'a resource' : `${leftovers.length} resources`}, released at once:\n${leftovers.join('\n')}`,
    );
  }

  /** Releases the open file's resources and closes it. */
  releaseFile(): void {
    const open = this.open;
    if (open === null) return;
    this.open = null;
    this.retired = open.file;
    for (const [handle, kind] of open.timers) this.clear(handle, kind);
    for (const [listener, entries] of open.listeners) {
      for (const { target, type, capture } of entries) target.removeEventListener(type, listener, capture);
    }
  }

  private clear(handle: Timer, kind: TimerKind): void {
    if (kind === 'timeout') this.real.clearTimeout(handle as ReturnType<typeof setTimeout>);
    else if (kind === 'interval') this.real.clearInterval(handle as ReturnType<typeof setInterval>);
    else this.real.clearImmediate(handle as ReturnType<typeof setImmediate>);
  }

  private leftover(what: string): void {
    // The installing code's frames, without this module's wrappers.
    const frames = (new Error().stack ?? '').split('\n').slice(1).filter((frame) => !frame.includes(`${THIS_FILE}:`));
    const where = frames.length > 0 ? frames.slice(0, 5).join('\n') : '    (no stack)';
    this.leftovers.push(`  ${what}, installed at\n${where}`);
  }

  private addTimer(handle: Timer, kind: TimerKind): void {
    if (this.open !== null) {
      this.open.timers.set(handle, kind);
      return;
    }
    this.clear(handle, kind);
    this.leftover(`a pending ${kind}`);
  }

  private deleteTimer(handle: unknown): void {
    this.open?.timers.delete(handle as Timer);
  }

  private listenerAdded(target: EventTarget, type: string, listener: EventListenerOrEventListenerObject, options: boolean | AddEventListenerOptions | undefined, remove: EventTarget['removeEventListener']): void {
    const open = this.open;
    if (open === null) {
      remove.call(target, type, listener, options);
      this.leftover(`a '${type}' listener`);
      return;
    }
    const entries = open.listeners.get(listener) ?? [];
    entries.push({ target, type, capture: captureOf(options) });
    open.listeners.set(listener, entries);
  }

  private listenerRemoved(target: EventTarget, type: string, listener: EventListenerOrEventListenerObject, options: boolean | EventListenerOptions | undefined): void {
    const entries = this.open?.listeners.get(listener);
    if (entries === undefined) return;
    const capture = captureOf(options);
    const index = entries.findIndex((entry) => entry.target === target && entry.type === type && entry.capture === capture);
    if (index >= 0) entries.splice(index, 1);
    if (entries.length === 0) this.open?.listeners.delete(listener);
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
      const handle = real.setTimeout(() => {
        deleted(handle);
        callback(...args);
      }, delay);
      added(handle, 'timeout');
      return handle;
    } as typeof globalThis.setTimeout;
    host.setInterval = function setInterval(callback: unknown, delay?: number, ...args: unknown[]) {
      const handle = real.setInterval(callback as (...a: unknown[]) => void, delay, ...args);
      added(handle, 'interval');
      return handle;
    } as typeof globalThis.setInterval;
    host.setImmediate = function setImmediate(callback: (...a: unknown[]) => void, ...args: unknown[]) {
      const handle = real.setImmediate(() => {
        deleted(handle);
        callback(...args);
      });
      added(handle, 'immediate');
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

type StartedImport = { readonly promise: Promise<unknown>; readonly label: string };

/** The dynamic imports each file starts. The import-tracking transform
 * (config/importTrackerPlugin.ts) hands every `import()` in a source or
 * test module to `track`, which returns the same promise. No handler is
 * attached until the file retires, so a rejection reaches its caller, and
 * one no caller handles is reported, exactly as without tracking. */
export class FileImports {
  private file = '';
  private started: StartedImport[] = [];
  private readonly pending = new Set<StartedImport>();

  openFile(file: string): void {
    this.file = file;
    this.started = [];
    this.pending.clear();
  }

  track<T>(promise: Promise<T>, label: string): Promise<T> {
    this.started.push({ promise, label });
    return promise;
  }

  /** Resolves once every import the open file started has settled,
   * including those started by the imports it waits for. */
  async settled(): Promise<void> {
    while (this.started.length > 0) {
      const started = this.started;
      this.started = [];
      await Promise.all(started.map((entry) => {
        this.pending.add(entry);
        const done = () => {
          this.pending.delete(entry);
        };
        return entry.promise.then(done, done);
      }));
    }
  }

  /** The imports still in flight, each with the file that started it. */
  inFlight(): string[] {
    return [...this.pending].map(({ label }) => `${label} (started by ${this.file})`);
  }
}
