export interface TerminalInputWriter {
  write(data: string): void;
  idle(): Promise<void>;
  dispose(): void;
}

export interface TerminalResizeWriter {
  resize(rows: number, cols: number): void;
  dispose(): void;
}

/**
 * Preserve keystroke order across an RPC transport that deliberately executes
 * calls concurrently. Only one write is in flight; input produced while it is
 * pending is concatenated into the next call so normal typing does not pay one
 * network round-trip per character.
 */
export function createTerminalInputWriter(
  send: (data: string) => Promise<unknown>,
  onError: (error: unknown) => void,
): TerminalInputWriter {
  let pending = '';
  let sending = false;
  let disposed = false;
  let idleWaiters: Array<() => void> = [];

  function resolveIdleWaiters(): void {
    if (sending || pending.length > 0) return;
    const waiters = idleWaiters;
    idleWaiters = [];
    for (const resolve of waiters) resolve();
  }

  async function drain(): Promise<void> {
    if (sending || disposed) return;
    sending = true;
    while (!disposed && pending.length > 0) {
      const next = pending;
      pending = '';
      try {
        await send(next);
      } catch (error) {
        onError(error);
      }
    }
    sending = false;
    resolveIdleWaiters();
  }

  return {
    write(data: string): void {
      if (disposed || data.length === 0) return;
      pending += data;
      void drain();
    },
    idle(): Promise<void> {
      if (!sending && pending.length === 0) return Promise.resolve();
      return new Promise((resolve) => idleWaiters.push(resolve));
    },
    dispose(): void {
      disposed = true;
      pending = '';
      resolveIdleWaiters();
    },
  };
}

/**
 * Serialize terminal resizes and collapse any backlog to the newest geometry.
 * Intermediate sizes have no value once a later layout measurement exists;
 * sending them concurrently can instead let an older RPC win at the PTY.
 */
export function createTerminalResizeWriter(
  send: (rows: number, cols: number) => Promise<unknown>,
  onError: (error: unknown) => void,
): TerminalResizeWriter {
  let pending: { rows: number; cols: number } | null = null;
  let lastCompleted: { rows: number; cols: number } | null = null;
  let sending = false;
  let disposed = false;

  async function drain(): Promise<void> {
    if (sending || disposed) return;
    sending = true;
    while (!disposed && pending !== null) {
      const next = pending;
      pending = null;
      if (lastCompleted?.rows === next.rows && lastCompleted.cols === next.cols) {
        continue;
      }
      try {
        await send(next.rows, next.cols);
        lastCompleted = next;
      } catch (error) {
        onError(error);
      }
    }
    sending = false;
  }

  return {
    resize(rows: number, cols: number): void {
      if (disposed || rows <= 0 || cols <= 0) return;
      if (pending?.rows === rows && pending.cols === cols) return;
      if (!sending && lastCompleted?.rows === rows && lastCompleted.cols === cols) return;
      pending = { rows, cols };
      void drain();
    },
    dispose(): void {
      disposed = true;
      pending = null;
    },
  };
}
