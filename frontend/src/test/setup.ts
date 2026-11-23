// Setup for the happy-dom projects: the polyfills happy-dom lacks and the
// between-test steps of config/testResets.ts, which the reset registry runs
// for the modules a test file loads. This file imports no app module, so a
// change to one selects only the test files that import it.
import './resetRegistry';
// Holds the panes the thread-pane-tracking transform records; registered
// here because no app module imports it.
import './openThreadPanes';
import '@testing-library/jest-dom/vitest';
import './helpers/firstDivergence';
import { installWaitForBackoff } from './waitForBackoff';
import { installRealTimeBudget } from './realTimeBudget';

installWaitForBackoff();
installRealTimeBudget();

if (typeof globalThis.ResizeObserver === 'undefined') {
  class StubResizeObserver {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
}

// Web Animations API stub. Svelte 5's built-in fade/scale/slide transitions
// call element.animate when a component that opts into them mounts. The stub
// returns an Animation-shaped object whose `finished` promise is already
// resolved, and which fires `finish` on the next microtask so Svelte's
// transition machinery completes without waiting on a clock.
//
// It replaces happy-dom's own implementation (added in 20.12): that one runs
// on timers, and its Animation.cancel() rejects `finished` with nothing
// handling it, which fails every transition torn down mid-animation
// (capricorn86/happy-dom#2339).
if (typeof Element !== 'undefined') {
  (Element.prototype as unknown as { animate: (...args: unknown[]) => Animation }).animate =
    function stubAnimate(): Animation {
      const handlers: { finish: Array<(e: Event) => void>; cancel: Array<(e: Event) => void> } = {
        finish: [],
        cancel: [],
      };
      const anim = {
        cancel() {
          for (const h of handlers.cancel) h(new Event('cancel'));
          if (typeof anim.oncancel === 'function') {
            anim.oncancel.call(anim as unknown as Animation, new Event('cancel'));
          }
        },
        finish() {
          for (const h of handlers.finish) h(new Event('finish'));
          if (typeof anim.onfinish === 'function') {
            anim.onfinish.call(anim as unknown as Animation, new Event('finish'));
          }
        },
        play() {},
        pause() {},
        reverse() {},
        addEventListener(type: string, cb: EventListenerOrEventListenerObject) {
          const fn = cb as (e: Event) => void;
          if (type === 'finish') handlers.finish.push(fn);
          else if (type === 'cancel') handlers.cancel.push(fn);
        },
        removeEventListener(type: string, cb: EventListenerOrEventListenerObject) {
          const fn = cb as (e: Event) => void;
          if (type === 'finish') handlers.finish = handlers.finish.filter((h) => h !== fn);
          else if (type === 'cancel') handlers.cancel = handlers.cancel.filter((h) => h !== fn);
        },
        dispatchEvent() { return true; },
        currentTime: 0,
        playState: 'finished' as AnimationPlayState,
        finished: Promise.resolve() as unknown as Animation['finished'],
        onfinish: null as ((this: Animation, ev: Event) => unknown) | null,
        oncancel: null as ((this: Animation, ev: Event) => unknown) | null,
      };
      // Fire finish on the next microtask so Svelte's transition runner
      // observes the "completed" state and tears down the DOM node in the
      // same tick a real animation would.
      queueMicrotask(() => anim.finish());
      return anim as unknown as Animation;
    };
}

// Companion to the animate stub: svelte's transition runner probes
// `element.getAnimations()` before dismantling an out-transitioning node
// (transitions.js `dispatch_event` path). Nothing in the stub ever registers
// a live animation, so the answer is always "none".
if (typeof Element !== 'undefined') {
  (Element.prototype as unknown as { getAnimations: () => Animation[] }).getAnimations =
    function stubGetAnimations(): Animation[] {
      return [];
    };
}

// happy-dom does not implement document.execCommand. The composer +
// textEditing dispatcher rely on it for `insertText` / `delete` so that
// programmatic edits participate in the browser's native undo stack.
// Provide a lightweight polyfill that mutates the active editable target
// and dispatches a synthetic `input` event so handlers downstream of
// `oninput` observe the change. The polyfill is enough for tests; the
// real browser implementation remains the source of truth for runtime
// behavior.
if (typeof document !== 'undefined' && typeof (document as { execCommand?: unknown }).execCommand !== 'function') {
  (document as unknown as { execCommand: (cmd: string, showUI?: boolean, value?: string) => boolean }).execCommand =
    (cmd: string, _showUI?: boolean, value?: string): boolean => {
      const target = document.activeElement;
      if (!(target instanceof HTMLTextAreaElement) && !(target instanceof HTMLInputElement)) return false;
      const el = target as HTMLTextAreaElement | HTMLInputElement;
      const current = el.value;
      const start = el.selectionStart ?? current.length;
      const end = el.selectionEnd ?? start;
      let inputType: string;
      let nextValue: string;
      let nextCaret: number;
      if (cmd === 'insertText') {
        inputType = 'insertText';
        nextValue = current.slice(0, start) + (value ?? '') + current.slice(end);
        nextCaret = start + (value?.length ?? 0);
      } else if (cmd === 'delete') {
        inputType = 'deleteContentBackward';
        if (start === end) {
          if (start === 0) return true;
          nextValue = current.slice(0, start - 1) + current.slice(end);
          nextCaret = start - 1;
        } else {
          nextValue = current.slice(0, start) + current.slice(end);
          nextCaret = start;
        }
      } else {
        return false;
      }
      el.value = nextValue;
      el.setSelectionRange(nextCaret, nextCaret);
      el.dispatchEvent(new Event('input', { bubbles: true, cancelable: false }));
      void inputType;
      return true;
    };
}

if (typeof globalThis.matchMedia === 'undefined') {
  globalThis.matchMedia = (() => ({
    matches: false,
    media: '',
    onchange: null,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent() { return false; },
  })) as unknown as typeof matchMedia;
}

// happy-dom and Node do not expose reliable Storage objects in every
// supported runtime. Node 24, for example, installs a host storage object
// whose methods are unusable without --localstorage-file. Give unit tests
// deterministic, independent local/session stores so storage behavior and
// failure injection do not depend on the host process.
function makeMemoryStorage(): Storage {
  const entries = new Map<string, string>();
  return {
    get length() { return entries.size; },
    clear(): void { entries.clear(); },
    getItem(key: string): string | null {
      return entries.has(key) ? (entries.get(key) as string) : null;
    },
    setItem(key: string, value: string): void { entries.set(key, String(value)); },
    removeItem(key: string): void { entries.delete(key); },
    key(index: number): string | null {
      return Array.from(entries.keys())[index] ?? null;
    },
  };
}

const memoryStorage = makeMemoryStorage();
const memorySessionStorage = makeMemoryStorage();
Object.defineProperty(globalThis, 'localStorage', {
  value: memoryStorage,
  configurable: true,
  writable: true,
});
Object.defineProperty(globalThis, 'sessionStorage', {
  value: memorySessionStorage,
  configurable: true,
  writable: true,
});
if (typeof globalThis.window !== 'undefined') {
  // Keep window storage in sync so components that reach through window
  // explicitly see the same stores as globalThis.
  Object.defineProperty(globalThis.window, 'localStorage', {
    value: memoryStorage,
    configurable: true,
    writable: true,
  });
  Object.defineProperty(globalThis.window, 'sessionStorage', {
    value: memorySessionStorage,
    configurable: true,
    writable: true,
  });
}
