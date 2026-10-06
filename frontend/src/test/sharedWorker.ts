// Setup for the `unit` project, whose workers run many files in one process
// against one happy-dom window (vitest.config.ts). The first setup file, so
// it tracks what setup.ts's imports install and its afterAll runs after
// setup.ts's hooks and the file's own. sharedWorkerStart.ts, the last setup
// file, records what the test file starts from.
//
// Each file leaves the worker as a fresh worker would find it:
//   - app modules are re-evaluated for the next file (`vi.resetModules`),
//     so module state does not carry over;
//   - listeners a file's modules added to `window` or `document`, and the
//     timers they left pending, end with the file, so the previous file's
//     module instances stop running and can be collected;
//   - the document body, focus, selection, URL and cookies are reset;
//   - a file that replaces a global, a DOM prototype member or a host
//     object property must put it back itself; sharedGlobals.ts fails the
//     file that leaves one changed.
import { afterAll, vi } from 'vitest';
import { sharedGlobalChanges, snapshotSharedGlobals, type SharedGlobalsSnapshot } from './sharedGlobals';

type TrackedListener = {
  readonly target: EventTarget;
  readonly type: string;
  readonly capture: boolean;
};

type FileResources = {
  readonly listeners: Map<EventListenerOrEventListenerObject, TrackedListener[]>;
  readonly timers: Set<ReturnType<typeof setTimeout>>;
};

const RESOURCES = Symbol.for('agent-overflow.test.sharedWorkerResources');

// happy-dom's control API on the environment's window.
const happyDOM = (window as unknown as { happyDOM: { abort(): Promise<void>; setURL(url: string): void } }).happyDOM;

function captureOf(options: boolean | EventListenerOptions | undefined): boolean {
  return typeof options === 'boolean' ? options : options?.capture === true;
}

function trackListeners(target: EventTarget, resources: FileResources): void {
  const add = target.addEventListener;
  const remove = target.removeEventListener;
  Object.defineProperty(target, 'addEventListener', {
    configurable: true,
    writable: true,
    value: function addEventListener(
      this: EventTarget,
      type: string,
      listener: EventListenerOrEventListenerObject | null,
      options?: boolean | AddEventListenerOptions,
    ): void {
      add.call(this, type, listener, options);
      if (listener === null) return;
      const entries = resources.listeners.get(listener) ?? [];
      entries.push({ target: this, type, capture: captureOf(options) });
      resources.listeners.set(listener, entries);
    },
  });
  Object.defineProperty(target, 'removeEventListener', {
    configurable: true,
    writable: true,
    value: function removeEventListener(
      this: EventTarget,
      type: string,
      listener: EventListenerOrEventListenerObject | null,
      options?: boolean | EventListenerOptions,
    ): void {
      remove.call(this, type, listener, options);
      if (listener === null) return;
      const entries = resources.listeners.get(listener);
      if (entries === undefined) return;
      const capture = captureOf(options);
      const index = entries.findIndex((entry) => entry.target === this && entry.type === type && entry.capture === capture);
      if (index >= 0) entries.splice(index, 1);
      if (entries.length === 0) resources.listeners.delete(listener);
    },
  });
}

function trackTimers(resources: FileResources): void {
  const { setTimeout: realSetTimeout, setInterval: realSetInterval } = globalThis;
  const { clearTimeout: realClearTimeout, clearInterval: realClearInterval } = globalThis;
  globalThis.setTimeout = function setTimeout(callback: unknown, delay?: number, ...args: unknown[]) {
    if (typeof callback !== 'function') return realSetTimeout(callback as () => void, delay);
    const handle = realSetTimeout(() => {
      resources.timers.delete(handle);
      callback(...args);
    }, delay);
    resources.timers.add(handle);
    return handle;
  } as typeof globalThis.setTimeout;
  globalThis.setInterval = function setInterval(callback: unknown, delay?: number, ...args: unknown[]) {
    const handle = realSetInterval(callback as (...a: unknown[]) => void, delay, ...args);
    resources.timers.add(handle);
    return handle;
  } as typeof globalThis.setInterval;
  globalThis.clearTimeout = function clearTimeout(handle) {
    if (handle != null) resources.timers.delete(handle as ReturnType<typeof setTimeout>);
    realClearTimeout(handle);
  } as typeof globalThis.clearTimeout;
  globalThis.clearInterval = function clearInterval(handle) {
    if (handle != null) resources.timers.delete(handle as ReturnType<typeof setTimeout>);
    realClearInterval(handle);
  } as typeof globalThis.clearInterval;
}

// Installed once per worker, before setup.ts imports any app module.
function workerResources(): FileResources {
  const holder = globalThis as { [RESOURCES]?: FileResources };
  const existing = holder[RESOURCES];
  if (existing !== undefined) return existing;
  const resources: FileResources = { listeners: new Map(), timers: new Set() };
  trackListeners(window, resources);
  trackListeners(document, resources);
  // happy-dom keeps a window resize listener for each MediaQueryList change
  // listener, out of reach of the window wrapper above.
  const matchMedia = window.matchMedia;
  window.matchMedia = function (this: Window, query: string): MediaQueryList {
    const list = matchMedia.call(this, query);
    trackListeners(list, resources);
    return list;
  };
  trackTimers(resources);
  holder[RESOURCES] = resources;
  return resources;
}

function releaseFileResources(resources: FileResources): void {
  for (const handle of resources.timers) clearTimeout(handle);
  resources.timers.clear();
  for (const [listener, entries] of resources.listeners) {
    for (const { target, type, capture } of [...entries]) target.removeEventListener(type, listener, capture);
  }
  resources.listeners.clear();
}

// A fresh window has an empty, unfocused body with no selection, no
// attributes on <html> or <body>, no cookies and the configured URL.
// <head> keeps the styles Svelte injects once per component module.
function resetDocument(href: string): void {
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
  if (location.href !== href) happyDOM.setURL(href);
}

const resources = workerResources();
let fileStart: { readonly globals: SharedGlobalsSnapshot; readonly href: string } | null = null;

/** Called by sharedWorkerStart.ts once setup.ts has run, before the test
 *  file is imported, so module-scope changes in the test file count. */
export function startSharedWorkerFile(): void {
  fileStart = { globals: snapshotSharedGlobals(), href: location.href };
}

afterAll(async () => {
  const start = fileStart;
  if (start === null) throw new Error('sharedWorkerStart.ts must be the last setup file of the unit project');
  // Stubs and spies taken over fake timers come off before the timers do.
  vi.unstubAllGlobals();
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
  // Vitest holds every mock that recorded a call, with its arguments,
  // until it is cleared.
  vi.clearAllMocks();
  vi.useRealTimers();
  releaseFileResources(resources);
  await happyDOM.abort();
  resetDocument(start.href);
  vi.resetModules();
  const changes = sharedGlobalChanges(start.globals);
  if (changes.length > 0) {
    throw new Error(
      `This file left shared globals changed for the next file in its worker; restore them in afterEach or afterAll, or use vi.stubGlobal: ${changes.join('; ')}`,
    );
  }
});
