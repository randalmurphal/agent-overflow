export interface AnimationFrameBatcher {
  request(callback: FrameRequestCallback): number;
  cancel(handle: number): void;
}

export type AnimationFramePhase =
  | 'before-dom-update'
  | 'dom-update';

// Every batcher is a named view onto this one coordinator. Parallel arrays
// avoid minting a request object on each display frame. The context and phase
// entries are references/primitives, while callback slots are nulled as they
// run so cancellation stays valid during dispatch.
let nextHandle = 0;
let nativeFrame: number | undefined;
let pendingCallbacks: Array<FrameRequestCallback | null> = [];
let pendingHandles: number[] = [];
let pendingContexts: string[] = [];
let pendingBeforeUpdate: boolean[] = [];
let pendingActive = 0;
let dispatchCallbacks: Array<FrameRequestCallback | null> = [];
let dispatchHandles: number[] = [];
let dispatchContexts: string[] = [];
let dispatchBeforeUpdate: boolean[] = [];
let dispatchTimestamp = 0;
let dispatchFailed = false;
let dispatchFirstError: unknown;

function allocateHandle(): number {
  candidate: while (true) {
    nextHandle = nextHandle >= Number.MAX_SAFE_INTEGER ? 1 : nextHandle + 1;
    for (let index = 0; index < pendingHandles.length; index++) {
      if (pendingCallbacks[index] !== null && pendingHandles[index] === nextHandle) {
        continue candidate;
      }
    }
    for (let index = 0; index < dispatchHandles.length; index++) {
      if (dispatchCallbacks[index] !== null && dispatchHandles[index] === nextHandle) {
        continue candidate;
      }
    }
    return nextHandle;
  }
}

function recordFailure(context: string, error: unknown): void {
  if (!dispatchFailed) {
    dispatchFailed = true;
    dispatchFirstError = error;
    return;
  }
  console.error(`[${context}] animation-frame callback failed`, error);
}

function runDispatchPhase(beforeDomUpdate: boolean): void {
  for (let index = 0; index < dispatchCallbacks.length; index++) {
    const callback = dispatchCallbacks[index];
    if (callback === null || dispatchBeforeUpdate[index] !== beforeDomUpdate) continue;
    // A callback earlier in this phase may cancel a later owner.
    dispatchCallbacks[index] = null;
    try {
      callback(dispatchTimestamp);
    } catch (error) {
      recordFailure(dispatchContexts[index], error);
    }
  }
}

function finishDispatch(): void {
  dispatchCallbacks.length = 0;
  dispatchHandles.length = 0;
  dispatchContexts.length = 0;
  dispatchBeforeUpdate.length = 0;
  if (!dispatchFailed) return;
  const error = dispatchFirstError;
  dispatchFailed = false;
  dispatchFirstError = undefined;
  throw error;
}

function runFrame(timestamp: DOMHighResTimeStamp): void {
  nativeFrame = undefined;
  const nextCallbacks = dispatchCallbacks;
  const nextHandles = dispatchHandles;
  const nextContexts = dispatchContexts;
  const nextBeforeUpdate = dispatchBeforeUpdate;
  dispatchCallbacks = pendingCallbacks;
  dispatchHandles = pendingHandles;
  dispatchContexts = pendingContexts;
  dispatchBeforeUpdate = pendingBeforeUpdate;
  pendingCallbacks = nextCallbacks;
  pendingHandles = nextHandles;
  pendingContexts = nextContexts;
  pendingBeforeUpdate = nextBeforeUpdate;
  pendingCallbacks.length = 0;
  pendingHandles.length = 0;
  pendingContexts.length = 0;
  pendingBeforeUpdate.length = 0;
  pendingActive = 0;
  dispatchTimestamp = timestamp;
  dispatchFailed = false;
  dispatchFirstError = undefined;

  runDispatchPhase(true);
  runDispatchPhase(false);
  finishDispatch();
}

function requestCoordinatedFrame(
  context: string,
  beforeDomUpdate: boolean,
  callback: FrameRequestCallback,
): number {
  const handle = allocateHandle();
  pendingCallbacks.push(callback);
  pendingHandles.push(handle);
  pendingContexts.push(context);
  pendingBeforeUpdate.push(beforeDomUpdate);
  pendingActive++;
  if (nativeFrame !== undefined) return handle;
  try {
    nativeFrame = requestAnimationFrame(runFrame);
  } catch (error) {
    // A failed registration must not leave a handle that can never fire.
    pendingCallbacks.pop();
    pendingHandles.pop();
    pendingContexts.pop();
    pendingBeforeUpdate.pop();
    pendingActive--;
    throw error;
  }
  return handle;
}

function cancelCoordinatedFrame(handle: number): void {
  for (let index = 0; index < pendingHandles.length; index++) {
    if (pendingCallbacks[index] !== null && pendingHandles[index] === handle) {
      pendingCallbacks[index] = null;
      pendingActive--;
      if (pendingActive === 0 && nativeFrame !== undefined) {
        cancelAnimationFrame(nativeFrame);
        nativeFrame = undefined;
        pendingCallbacks.length = 0;
        pendingHandles.length = 0;
        pendingContexts.length = 0;
        pendingBeforeUpdate.length = 0;
      }
      return;
    }
  }
  for (let index = 0; index < dispatchHandles.length; index++) {
    if (dispatchCallbacks[index] !== null && dispatchHandles[index] === handle) {
      dispatchCallbacks[index] = null;
      return;
    }
  }
}

/**
 * Coalesces every independent owner onto one native rAF callback while
 * preserving per-owner cancellation and next-frame scheduling semantics.
 *
 * Geometry-sensitive scroll owners run first. Chat springs consume the
 * virtualizer's prior layout sample, so a same-frame reveal cannot improve
 * their target. Running the spring before direct or Svelte DOM updates keeps
 * its scrollTop write on clean layout. DOM updates then land before the frame
 * paints. This avoids turning each reveal into a synchronous layout flush.
 */
export function createAnimationFrameBatcher(
  errorContext: string,
  phase: AnimationFramePhase = 'dom-update',
): AnimationFrameBatcher {
  const beforeDomUpdate = phase === 'before-dom-update';
  return {
    request(callback) {
      return requestCoordinatedFrame(errorContext, beforeDomUpdate, callback);
    },
    cancel: cancelCoordinatedFrame,
  };
}

/** Test seam for suites that replace the global rAF queue between cases. */
export function __resetAnimationFrameCoordinatorForTest(): void {
  if (nativeFrame !== undefined) cancelAnimationFrame(nativeFrame);
  nativeFrame = undefined;
  pendingCallbacks.length = 0;
  pendingHandles.length = 0;
  pendingContexts.length = 0;
  pendingBeforeUpdate.length = 0;
  pendingActive = 0;
  dispatchCallbacks.length = 0;
  dispatchHandles.length = 0;
  dispatchContexts.length = 0;
  dispatchBeforeUpdate.length = 0;
  dispatchFailed = false;
  dispatchFirstError = undefined;
  nextHandle = 0;
}
