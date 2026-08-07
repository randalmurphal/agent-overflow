// Types for svelteInternalRuntime.js — see that file for why it is JS.

/** svelte's `queue_micro_task`. While `is_flushing_sync` it appends directly
 * to the drain's array, which is the loop the flush-caps hunk bounds. */
export declare const queueSvelteMicroTask: (fn: () => void) => void;

/** svelte's own effect flags, read rather than transcribed so a renumbering
 * fails the suite instead of silently testing nothing. */
export declare const DESTROYED_FLAG: number;
export declare const SETTLED_BOUNDARY_FLAGS: number;

/**
 * svelte's `invoke_error_boundary`, typed to the contract the patch adds:
 * `true` when a boundary took the error, `false` when it was DECLINED
 * because the anchor effect is destroyed (the pristine build returns
 * `undefined` in both cases, which is the silent swallow). Every other
 * outcome throws.
 */
export declare const invokeErrorBoundary: (error: unknown, effect: unknown) => boolean;

/** `flush-caps.js#FLUSH_SYNC_MAX_LAPS`, read from the patch rather than copied. */
export declare const FLUSH_SYNC_MAX_LAPS_PATCHED: number;
/** `flush-caps.js#FLUSH_TASKS_MAX_TASKS`, read from the patch rather than copied. */
export declare const FLUSH_TASKS_MAX_TASKS_PATCHED: number;
