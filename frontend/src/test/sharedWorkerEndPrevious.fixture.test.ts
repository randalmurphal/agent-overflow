import { describe, it } from 'vitest';

// A file that runs no test: every case below is skipped, so Vitest marks the
// whole file skipped and runs none of its hooks (no beforeAll, afterAll or
// fixture cleanup). sharedWorker.ts's afterAll therefore does not end it, and
// without sharedWorkerEndPrevious.ts the next file in the worker would load
// its setup files' imports from this file's module instances: the reset
// registry with this file's between-test hooks, which never run in the next
// file. sharedWorkerEndPrevious.test.ts runs this file in a child Vitest, in
// one worker, before openThreadPanes.test.ts. Without a results cache Vitest
// runs the larger of two files first, so this file stays the larger.
describe.skip('a skipped suite', () => {
  it('runs nothing', () => {});
});

it.skip('a skipped test', () => {});

it.todo('a test still to write');
