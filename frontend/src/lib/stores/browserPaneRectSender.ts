// Ordered delivery of a browser pane's host rect to the backend.
//
// Each mount has at most one report in flight and keeps only the newest
// report behind it, so reports land in order by construction (the backend
// runs each call on its own goroutine, last writer wins) and a drag cannot
// fill the transport's in-flight cap. A report equal to the last one the
// backend accepted is not sent; the key is recorded only once a send
// succeeds, so a failed report is never mistaken for a delivered one. A
// failure is retried, bounded, with the newest report: a lost final report
// would leave the native view where the pane no longer is. A refusal that
// says the mount is gone ends the mount's reports (the pane is tearing down,
// and a new attach reports afresh).

export interface BrowserPaneRectReport {
  x: number;
  y: number;
  width: number;
  height: number;
  clipX: number;
  clipY: number;
  clipWidth: number;
  clipHeight: number;
  viewportWidth: number;
  viewportHeight: number;
  devicePixelRatio: number;
  visible: boolean;
  background: string;
}

export interface BrowserPaneRectSenderOptions {
  send: (mount: string, rect: BrowserPaneRectReport) => Promise<unknown>;
  /** A report that still failed after every retry. */
  onFailure: (mount: string, error: unknown) => void;
  /** Whether a refusal means the backend no longer has the mount. */
  isMountGone: (error: unknown) => boolean;
  maxRetries?: number;
  retryDelayMs?: number;
}

export interface BrowserPaneRectSender {
  report(mount: string, rect: BrowserPaneRectReport): void;
  /** Drops the mount's state; an answer still in flight is ignored. */
  forget(mount: string): void;
}

interface Pending {
  key: string;
  rect: BrowserPaneRectReport;
}

interface MountState {
  /** The report being sent, or waiting to be retried. */
  current: Pending | null;
  /** The newest report that arrived while `current` was out. */
  next: Pending | null;
  /** The last report the backend accepted. */
  acceptedKey: string;
  failures: number;
  retryTimer: ReturnType<typeof setTimeout> | null;
}

export function browserPaneRectKey(r: BrowserPaneRectReport): string {
  return (
    `${r.x},${r.y},${r.width},${r.height},${r.clipX},${r.clipY},${r.clipWidth},${r.clipHeight},` +
    `${r.viewportWidth},${r.viewportHeight},${r.devicePixelRatio},${r.visible},${r.background}`
  );
}

export function createBrowserPaneRectSender(options: BrowserPaneRectSenderOptions): BrowserPaneRectSender {
  const maxRetries = options.maxRetries ?? 3;
  const retryDelayMs = options.retryDelayMs ?? 100;
  const mounts = new Map<string, MountState>();

  function start(mount: string, state: MountState, item: Pending): void {
    state.current = item;
    let sent: Promise<unknown>;
    try {
      sent = options.send(mount, item.rect);
    } catch (error) {
      sent = Promise.reject(error);
    }
    sent.then(
      () => settled(mount, state, item, null),
      (error: unknown) => settled(mount, state, item, error ?? new Error('browser pane rect report failed')),
    );
  }

  function settled(mount: string, state: MountState, item: Pending, error: unknown): void {
    if (mounts.get(mount) !== state) return;
    if (error === null) {
      state.acceptedKey = item.key;
      state.failures = 0;
      state.current = null;
      sendNext(mount, state);
      return;
    }
    if (options.isMountGone(error)) {
      mounts.delete(mount);
      return;
    }
    state.failures += 1;
    if (state.failures > maxRetries) {
      state.failures = 0;
      state.current = null;
      options.onFailure(mount, error);
      sendNext(mount, state);
      return;
    }
    // Retry with the newest report: an older one is only worth sending if
    // nothing replaced it.
    state.retryTimer = setTimeout(
      () => {
        state.retryTimer = null;
        if (mounts.get(mount) !== state) return;
        const retry = state.next ?? item;
        state.next = null;
        start(mount, state, retry);
      },
      retryDelayMs * 2 ** (state.failures - 1),
    );
  }

  function sendNext(mount: string, state: MountState): void {
    const next = state.next;
    state.next = null;
    if (next && next.key !== state.acceptedKey) start(mount, state, next);
  }

  return {
    report(mount, rect) {
      const item = { key: browserPaneRectKey(rect), rect: { ...rect } };
      let state = mounts.get(mount);
      if (!state) {
        state = { current: null, next: null, acceptedKey: '', failures: 0, retryTimer: null };
        mounts.set(mount, state);
      }
      if (state.current) {
        // The one out is already this report: nothing newer to queue.
        state.next = state.current.key === item.key ? null : item;
        return;
      }
      if (item.key === state.acceptedKey) return;
      start(mount, state, item);
    },
    forget(mount) {
      const state = mounts.get(mount);
      if (state?.retryTimer) clearTimeout(state.retryTimer);
      mounts.delete(mount);
    },
  };
}
