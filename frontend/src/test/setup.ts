import '@testing-library/jest-dom/vitest';
import { afterEach, beforeEach } from 'vitest';
import { cleanup } from '@testing-library/svelte';
import { resetWailsMocks } from './mocks/wailsio-runtime';
import { resetBindingMocks } from './mocks/bindings-app';
import { resetForTest as resetThreadStatusesForTest } from '../lib/stores/threadStatuses.svelte';
import { resetDiffReviewCommentsForTest } from '../lib/stores/diffReviewComments.svelte';
import {
  __resetActivityRailUiPrefsForTest,
  __resetLiveTodoUiPrefsForTest,
} from '../lib/stores/liveTodoState.svelte';
import { __resetPayloadCacheForTest } from '../lib/utils/payloadDataCache';
import { clearThreadItemCacheForTest } from '../lib/stores/threadItemCache';
import { resetProviderModelsForTest } from '../lib/stores/providerModels.svelte';
import { clearAllThreadSizePriorsForTest } from '../lib/utils/virtual/priors';
import { __resetSizePriorsStorageForTest } from '../lib/utils/virtual/priorsStorage';
import { __setTransportStatusForTest } from '../lib/stores/transportStatus.svelte';
import { __resetGitStatusStoreForTest } from '../lib/stores/gitStatusStore.svelte';
import { __resetPRReviewStoreForTest } from '../lib/stores/prReviewStore.svelte';
import { __resetMcpServersStoreForTest } from '../lib/stores/mcpServers.svelte';
import { __resetChatBarFavoritesForTest } from '../lib/stores/chatBarFavorites.svelte';
import { __resetWorkspaceChangeLockForTest } from '../lib/stores/workspaceChangeLock.svelte';
import { __resetThreadHistoryStampsForTest } from '../lib/stores/threadHistoryStamps';
import { __resetReplicaForTest } from '../lib/replica';
import { __resetBackendIdentityForTest } from '../lib/transport/backendIdentity';

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

// Companion to the animate stub: svelte's transition runner probes
// `element.getAnimations()` before dismantling an out-transitioning node
// (transitions.js `dispatch_event` path), and happy-dom lacks it too.
// Nothing in the stub ever registers a live animation, so the honest
// answer is always "none".
if (typeof Element !== 'undefined' && typeof Element.prototype.getAnimations !== 'function') {
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

beforeEach(() => {
  // The unit suite has no transport, so the wsClient's module-load snapshot
  // is `disconnected` — a state no test means to exercise, and one that
  // suspends every connection-gated store. Pin it connected; a test that
  // cares about an outage drives it itself.
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  // Entity-keyed git status is a module-level singleton shared by every
  // pane, so it outlives a test. Reset after the transport pin, which
  // itself re-sources whatever the previous test left attached.
  __resetGitStatusStoreForTest();
  // Same shape for PR review state: one poll pump, CI pipeline and merge
  // tree per PR, shared by every pane — and by every test in a file.
  __resetPRReviewStoreForTest();
  // MCP rows are keyed by project (Claude) / app (Codex), chat-bar favorites
  // by the app, and the workspace-change lock by thread — all module-level
  // singletons that outlive a test the same way.
  __resetMcpServersStoreForTest();
  __resetChatBarFavoritesForTest();
  __resetWorkspaceChangeLockForTest();
});

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
  // History stamps, the replica session and the backend identity that
  // keys it are module-level singletons that outlive a test the same
  // way the item cache does.
  __resetThreadHistoryStampsForTest();
  __resetReplicaForTest();
  __resetBackendIdentityForTest();
  // Shared provider model metadata is a real app cache. Tests mock the
  // catalog per case, so stale model capabilities from another suite make
  // menus lie about context windows and fast-mode support.
  resetProviderModelsForTest();
  // Any component test that imports timelineSizePriors.svelte.ts installs
  // the real localStorage-backed size-priors adapter at module scope
  // (installSizePriorsPersistence runs on import). Reset its debounce
  // timer, dirty set, and disabled flag before wiping localStorage below —
  // otherwise a pending ~1s flush from this test can fire mid-way through
  // a LATER test in the same file and write stale entries into its
  // localStorage.
  clearAllThreadSizePriorsForTest();
  __resetSizePriorsStorageForTest();
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
