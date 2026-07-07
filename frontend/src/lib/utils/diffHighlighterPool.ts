// Main-thread wrapper around the Shiki tokenizer worker.
//
// Single worker instance for the whole app. Lazy-boots on first
// request; idle-terminates after a quiet period. On termination,
// in-flight Promises reject and a subsequent request reboots.
//
// Theme-mismatched responses are discarded to handle the race where
// the user toggles theme while tokens are in flight.

import type { LineToken } from './tokenCache';

export type DiffTheme = 'github-dark' | 'github-light';

interface TokenizeRequest {
  lines: string[];
  lang: string;
  theme: DiffTheme;
}

interface PendingEntry {
  resolve: (lines: LineToken[][]) => void;
  reject: (err: Error) => void;
  theme: DiffTheme;
}

const IDLE_TERMINATE_MS = 5 * 60 * 1000; // 5 minutes

// Hooks for tests — by default we construct from the module path.
export interface DiffHighlighterPoolOptions {
  workerFactory?: () => Worker;
  idleTerminateMs?: number;
}

export interface DiffHighlighterPool {
  tokenize(req: TokenizeRequest): Promise<LineToken[][]>;
  terminate(): void;
  /** True only while a worker is alive. Used by debug/diagnostics. */
  readonly isActive: boolean;
}

function defaultWorkerFactory(): Worker {
  // Vite resolves this URL at build time and inlines the worker
  // chunk. The `?worker` syntax also works but `new Worker(new URL(...))`
  // is the modern recommended form.
  return new Worker(
    new URL('../workers/diffHighlighter.worker.ts', import.meta.url),
    { type: 'module' },
  );
}

export function createDiffHighlighterPool(opts: DiffHighlighterPoolOptions = {}): DiffHighlighterPool {
  const factory = opts.workerFactory ?? defaultWorkerFactory;
  const idleMs = opts.idleTerminateMs ?? IDLE_TERMINATE_MS;

  let worker: Worker | null = null;
  let nextRequestId = 1;
  const pending = new Map<number, PendingEntry>();
  let idleTimer: ReturnType<typeof setTimeout> | null = null;

  function clearIdleTimer(): void {
    if (idleTimer !== null) {
      clearTimeout(idleTimer);
      idleTimer = null;
    }
  }

  function scheduleIdleTermination(): void {
    clearIdleTimer();
    if (pending.size > 0) return;
    idleTimer = setTimeout(() => {
      idleTimer = null;
      if (pending.size === 0) {
        terminateInternal();
      }
    }, idleMs);
  }

  function ensureWorker(): Worker {
    if (worker) return worker;
    const w = factory();
    w.addEventListener('message', (event: MessageEvent<unknown>) => {
      handleMessage(event.data);
    });
    w.addEventListener('error', (event: ErrorEvent) => {
      // Reject all in-flight; the next request will boot a new worker.
      // `terminateInternal` walks `pending` and rejects with a generic
      // "worker terminated" error — but for a worker-error path we
      // want the actual error message to surface, so reject explicitly
      // before terminating clears the map.
      const err = new Error(event.message || 'worker error');
      for (const [, entry] of pending) {
        entry.reject(err);
      }
      pending.clear();
      terminateInternal();
    });
    worker = w;
    return w;
  }

  function handleMessage(data: unknown): void {
    if (!data || typeof data !== 'object') return;
    const msg = data as {
      id?: number;
      kind?: string;
      theme?: string;
      tokens?: LineToken[][];
      error?: string;
    };
    if (typeof msg.id !== 'number') return;
    const entry = pending.get(msg.id);
    if (!entry) return;
    pending.delete(msg.id);

    if (msg.kind === 'error') {
      entry.reject(new Error(msg.error || 'tokenize error'));
    } else if (msg.kind === 'tokens') {
      // Theme-mismatched responses get dropped — the caller already
      // dispatched a follow-up for the new theme, and applying tokens
      // from the wrong palette would mis-render.
      if (msg.theme !== entry.theme) {
        entry.resolve(emptyTokens(msg.tokens?.length ?? 0));
      } else {
        entry.resolve(msg.tokens ?? []);
      }
    } else {
      // Unrecognised envelope — never silently strand the caller.
      entry.reject(new Error(`unrecognised tokenizer response kind: ${msg.kind ?? '<missing>'}`));
    }

    if (pending.size === 0) {
      scheduleIdleTermination();
    }
  }

  function emptyTokens(n: number): LineToken[][] {
    const out: LineToken[][] = [];
    for (let i = 0; i < n; i += 1) out.push([]);
    return out;
  }

  function terminateInternal(): void {
    clearIdleTimer();
    if (worker) {
      worker.terminate();
      worker = null;
    }
    // Reject any orphaned requests.
    for (const [, entry] of pending) {
      entry.reject(new Error('worker terminated'));
    }
    pending.clear();
  }

  return {
    get isActive(): boolean {
      return worker !== null;
    },
    tokenize(req: TokenizeRequest): Promise<LineToken[][]> {
      const w = ensureWorker();
      clearIdleTimer();
      const id = nextRequestId++;
      return new Promise<LineToken[][]>((resolve, reject) => {
        pending.set(id, { resolve, reject, theme: req.theme });
        w.postMessage({
          id,
          kind: 'tokenize',
          lines: req.lines,
          lang: req.lang,
          theme: req.theme,
        });
      });
    },
    terminate(): void {
      terminateInternal();
    },
  };
}

// Module-shared instance for the app. Tests can construct their own
// via createDiffHighlighterPool({ workerFactory }).
let sharedPool: DiffHighlighterPool | null = null;

export function getSharedDiffHighlighterPool(): DiffHighlighterPool {
  sharedPool ??= createDiffHighlighterPool();
  return sharedPool;
}

export function resetSharedDiffHighlighterPoolForTest(): void {
  sharedPool?.terminate();
  sharedPool = null;
}
