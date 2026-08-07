// Deep reads into svelte's private runtime for the "flush-loop-caps"
// regression suite (svelte-patch-flush-caps.test.ts).
//
// The hunk lives in `dom/task.js` and `error-handling.js`, and svelte's
// exports map publishes neither (`svelte/internal/client` re-exports only a
// curated surface, and `queue_micro_task` / `invoke_error_boundary` are not
// on it). The suite therefore reaches them by FILE PATH. Vite resolves these
// to the same absolute module ids svelte itself imports, so the task queued
// here is the very array `flushSync`'s drain walks — which is what makes the
// cap observable at all.
//
// This file is JS, with its types in the sibling `.d.ts`, for one reason:
// TypeScript will not infer types across a `node_modules` JS import
// (`maxNodeModuleJsDepth` is 0), and the alternatives are worse — a
// project-wide compiler-option change, a `@ts-expect-error` per import, or a
// synthetic alias in `vitest.config.ts`. `checkJs` is false, so this file is
// simply not type-checked, and the `.d.ts` states the contract instead.
//
// Kept in one place so exactly one file knows the deep paths, and so the
// `.svelte.ts` cycle fixtures stay clean: the svelte compiler rejects
// `svelte/internal/*` imports in files it compiles, so internals are passed
// into those helpers as callbacks (same shape as
// `ownerlessRootHelpers.svelte.ts`).

import {
  BOUNDARY_EFFECT,
  DESTROYED,
  REACTION_RAN,
} from '../../../../node_modules/svelte/src/internal/client/constants.js';
import { queue_micro_task } from '../../../../node_modules/svelte/src/internal/client/dom/task.js';
import { invoke_error_boundary } from '../../../../node_modules/svelte/src/internal/client/error-handling.js';
import {
  FLUSH_SYNC_MAX_LAPS,
  FLUSH_TASKS_MAX_TASKS,
} from '../../../../node_modules/svelte/src/internal/client/flush-caps.js';

export const queueSvelteMicroTask = queue_micro_task;

export const DESTROYED_FLAG = DESTROYED;
export const SETTLED_BOUNDARY_FLAGS = BOUNDARY_EFFECT | REACTION_RAN;

export const invokeErrorBoundary = invoke_error_boundary;

// Read from the patch, never transcribed: an assertion like
// `expect(ran).toBe(FLUSH_TASKS_MAX_TASKS)` only tests the patch if the
// constant IS the patch's. A copy in the suite would keep passing against a
// re-tuned cap, which is the one thing it exists to notice.
export const FLUSH_SYNC_MAX_LAPS_PATCHED = FLUSH_SYNC_MAX_LAPS;
export const FLUSH_TASKS_MAX_TASKS_PATCHED = FLUSH_TASKS_MAX_TASKS;
