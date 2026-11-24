// The fixture files of sharedWorkerRunner.test.ts, by the child run that
// runs them, each in its order (vitest.config.ts here sorts by it).

/** One worker takes these one run request per file, as each worker of a
 * run with more than one does: the first holds the other worker until the
 * last is done. Each fixture leaves something behind, and the file after
 * it checks that nothing of it is active. */
export const RUN_FIXTURES = [
  'holdsWorker.fixture.ts',
  'leavesGlobal.fixture.ts',
  'leavesElement.fixture.ts',
  'afterLeavesElement.fixture.ts',
  'leavesImmediate.fixture.ts',
  'afterLeavesImmediate.fixture.ts',
  'afterTripwire.fixture.ts',
  'skipped.fixture.ts',
  'afterSkipped.fixture.ts',
  'failsLoading.fixture.ts',
  'afterFailsLoading.fixture.ts',
  'lateImport.fixture.ts',
  'afterLateImport.fixture.ts',
  'rejectsImport.fixture.ts',
  'releasesWorker.fixture.ts',
] as const;

/** Run with the default reporter: each fails only at its retirement. */
export const RETIREMENT_FAILURES = ['leavesGlobal.fixture.ts', 'skippedLeavesGlobal.fixture.ts'] as const;

/** Collected without a run, in one worker. */
export const COLLECT_FIXTURES = ['collectsListener.fixture.ts', 'afterCollectsListener.fixture.ts'] as const;

export const FIXTURE_ORDER: readonly string[] = [...new Set([...RUN_FIXTURES, ...RETIREMENT_FAILURES, ...COLLECT_FIXTURES])];
