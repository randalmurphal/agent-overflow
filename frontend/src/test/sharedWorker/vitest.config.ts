// The fixture files of sharedWorkerRunner.test.ts, run in the `unit`
// project's setting, in the order below, in one worker that takes them one
// run request at a time, as each worker of a run with more than one does.
// They are not test files, so no other run collects them.
import { resolve } from 'node:path';
import { defineConfig } from 'vitest/config';
import { BaseSequencer, type TestSpecification } from 'vitest/node';
import { unitProject } from '../../../vitest.config.ts';

export const FIXTURE_ORDER = [
  // Holds the other worker until the last fixture is done.
  'holdsWorker.fixture.ts',
  'leavesGlobal.fixture.ts',
  'leavesElement.fixture.ts',
  'afterLeavesElement.fixture.ts',
  'afterTripwire.fixture.ts',
  'skipped.fixture.ts',
  'afterSkipped.fixture.ts',
  'failsLoading.fixture.ts',
  'afterFailsLoading.fixture.ts',
  'lateImport.fixture.ts',
  'afterLateImport.fixture.ts',
];

class FixtureOrder extends BaseSequencer {
  override async sort(files: TestSpecification[]): Promise<TestSpecification[]> {
    return FIXTURE_ORDER.map((name) => {
      const file = files.find((spec) => spec.moduleId.endsWith(`/${name}`));
      if (file === undefined) throw new Error(`fixture ${name} was not collected`);
      return file;
    });
  }
}

export default defineConfig({
  ...unitProject,
  root: resolve(import.meta.dirname, '../../..'),
  test: {
    ...unitProject.test,
    name: 'shared-worker-fixtures',
    include: FIXTURE_ORDER.map((name) => `src/test/sharedWorker/${name}`),
    exclude: [],
    // One process: a timeout that stops the run stops every worker. One
    // worker gets every file in one request, so there are two, and the
    // first fixture holds one of them.
    pool: 'threads',
    maxWorkers: 2,
    sequence: { sequencer: FixtureOrder },
  },
});
