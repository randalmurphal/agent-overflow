// The first setup file of the `unit` project (see sharedWorker.ts): it ends
// the previous file in this worker before this file loads a module.
//
//   - Vitest runs no afterAll in a file that runs no test (every test
//     skipped, or the file failed to load), so that file's end step is
//     still waiting; it runs here.
//   - The previous file's reset registry ends, so a module that its
//     leftover work loads from now on is held for this file's registry
//     (resetRegistry.ts).
//   - Every module is reset, so this file evaluates its own instances and
//     none that the previous file, or work it left running, evaluated.
//
// Importing sharedWorker.ts here would evaluate it a second time in this
// file, so its end step is reached through the worker global.
import { vi } from 'vitest';
import { RESET_REGISTRY_KEY } from './config/testResets';
import type { ResetRegistry } from './resetRegistry';

const UNENDED_FILE = Symbol.for('agent-overflow.test.sharedWorkerUnendedFile');
const worker = globalThis as { [key: symbol]: (() => Promise<void>) | ResetRegistry | undefined };

await (worker[UNENDED_FILE] as (() => Promise<void>) | undefined)?.();
(worker[Symbol.for(RESET_REGISTRY_KEY)] as ResetRegistry | undefined)?.end();
vi.resetModules();
