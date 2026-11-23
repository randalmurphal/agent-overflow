// The first setup file of the `unit` project (see sharedWorker.ts). Vitest
// runs no afterAll in a file that runs no test (every test skipped, or the
// file failed to load), so that file's end step is still waiting when the
// next file in its worker starts. It runs here, before this file loads a
// module, so this file does not get that file's module instances, between-
// test hooks, listeners and timers. This file imports nothing: importing
// sharedWorker.ts here would evaluate it a second time in this file.
const UNENDED_FILE = Symbol.for('agent-overflow.test.sharedWorkerUnendedFile');

export {};

await (globalThis as { [UNENDED_FILE]?: () => Promise<void> })[UNENDED_FILE]?.();
