export interface TimelineTargetFlash {
  readonly itemId: string | null;
  readonly nonce: number;
  flash(itemId: string): void;
  clear(): void;
}

export function createTimelineTargetFlash(durationMs: number): TimelineTargetFlash {
  let itemId: string | null = $state(null);
  let nonce = $state(0);
  let timer: ReturnType<typeof setTimeout> | null = null;

  function clear(): void {
    if (timer) {
      clearTimeout(timer);
      timer = null;
    }
    itemId = null;
  }

  function flash(nextItemId: string): void {
    clear();
    nonce += 1;
    itemId = nextItemId;
    timer = setTimeout(() => {
      if (itemId === nextItemId) itemId = null;
      timer = null;
    }, durationMs);
  }

  return {
    get itemId() { return itemId; },
    get nonce() { return nonce; },
    flash,
    clear,
  };
}
