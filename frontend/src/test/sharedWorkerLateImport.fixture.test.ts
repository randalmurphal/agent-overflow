import { describe, it } from 'vitest';
import * as loaded from './openThreadPanes';

// Leaves work running past the end of this file, and that work loads a
// module once the modules were reset: the shape of a lazy chunk whose load
// outlives the test that started it. sharedWorkerEndPrevious.test.ts runs
// this file in a child Vitest, in one worker, before openThreadPanes.test.ts,
// whose between-test step belongs to the module this work loads. Skipped in
// every other run. Without a results cache Vitest runs the larger of two
// files first, so this file stays the larger.
describe.runIf(process.env.AO_LATE_IMPORT_FIXTURE === '1')('work left running', () => {
  it('loads a module after the file ended', () => {
    const poll = async (): Promise<void> => {
      if ((await import('./openThreadPanes')) !== loaded) return;
      setImmediate(() => void poll());
    };
    setImmediate(() => void poll());
  });
});
