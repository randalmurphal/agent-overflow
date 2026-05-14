import '@testing-library/jest-dom/vitest';
import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/svelte';
import { resetWailsMocks } from './mocks/wailsio-runtime';
import { resetBindingMocks } from './mocks/bindings-app';
import { resetForTest as resetThreadStatusesForTest } from '../lib/stores/threadStatuses.svelte';
import { resetDiffReviewCommentsForTest } from '../lib/stores/diffReviewComments.svelte';
import {
  __resetActivityRailUiPrefsForTest,
  __resetLiveTodoUiPrefsForTest,
} from '../lib/stores/thread.svelte';
import { __resetPayloadCacheForTest } from '../lib/utils/payloadDataCache';
import { clearThreadItemCacheForTest } from '../lib/stores/threadItemCache';
import { resetProviderModelsForTest } from '../lib/stores/providerModels.svelte';

if (typeof globalThis.ResizeObserver === 'undefined') {
  class StubResizeObserver {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
}

// Web Animations API stub. happy-dom does not implement element.animate,
// so Svelte 5's built-in fade/scale/slide transitions throw when a
// component that opts into them mounts under test. The stub returns an
// Animation-shaped object whose `finished` promise is already resolved,
// and which synchronously fires `finish` via onfinish so Svelte's
// transition machinery completes on the next microtask (matching the
// behavior tests expected back when transitions were effectively no-ops
// because happy-dom silently swallowed the missing method).
if (typeof Element !== 'undefined' && typeof Element.prototype.animate !== 'function') {
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

// happy-dom exposes `localStorage` as an empty object without the Storage
// prototype methods attached, so tests that exercise code using
// localStorage hit "setItem is not a function". Install a minimal
// in-memory Storage implementation once, and clear it between tests in
// the afterEach below so specs don't leak state at each other.
const __storage = new Map<string, string>();
const memoryStorage: Storage = {
  get length() { return __storage.size; },
  clear(): void { __storage.clear(); },
  getItem(key: string): string | null {
    return __storage.has(key) ? (__storage.get(key) as string) : null;
  },
  setItem(key: string, value: string): void { __storage.set(key, String(value)); },
  removeItem(key: string): void { __storage.delete(key); },
  key(index: number): string | null {
    return Array.from(__storage.keys())[index] ?? null;
  },
};
Object.defineProperty(globalThis, 'localStorage', {
  value: memoryStorage,
  configurable: true,
  writable: true,
});
if (typeof globalThis.window !== 'undefined') {
  // Keep window.localStorage in sync so components that reach through
  // window explicitly see the same store as globalThis.localStorage.
  Object.defineProperty(globalThis.window, 'localStorage', {
    value: memoryStorage,
    configurable: true,
    writable: true,
  });
}

afterEach(() => {
  resetDiffReviewCommentsForTest();
  cleanup();
  resetWailsMocks();
  resetBindingMocks();
  // The thread-statuses store holds the global active-turn registry
  // that backs the activity rail's working timer + sidebar pills. It's
  // $state-backed and shared across all panes, so test-to-test leaks
  // would surface as "this test sees a turn in flight from the previous
  // test" failures.
  resetThreadStatusesForTest();
  // Per-thread UI pref maps for the live-todo dropdown and the
  // activity rail's section toggles are module-scoped and survive
  // thread switches by design — clear them between tests so a
  // previous test's "I had Todos open on thread-1" doesn't leak into
  // the next test's fresh pane.
  __resetLiveTodoUiPrefsForTest();
  __resetActivityRailUiPrefsForTest();
  // The module-level payload-data cache survives thread switches by
  // design (the whole point — re-entering a thread renders synchronously
  // from cache instead of replaying empty-then-loaded). Reset it
  // between tests so a previous test's "thread-1/payload-1" hit doesn't
  // skip the next test's expected fetch.
  __resetPayloadCacheForTest();
  // Same shape: the per-thread items snapshot LRU survives switchThread
  // by design so re-entering a thread paints from cache. A previous
  // test's snapshot for thread-1 would otherwise let the next test's
  // pane skip its mocked load entirely.
  clearThreadItemCacheForTest();
  // Shared provider model metadata is a real app cache. Tests mock the
  // catalog per case, so stale model capabilities from another suite make
  // menus lie about context windows and fast-mode support.
  resetProviderModelsForTest();
  // Wipe the in-memory localStorage between tests so persistence-aware
  // stores don't leak state across suites.
  try {
    if (typeof localStorage !== 'undefined' && typeof localStorage.clear === 'function') {
      localStorage.clear();
    }
  } catch {
    // ignore — tests running without localStorage continue happily.
  }
});
