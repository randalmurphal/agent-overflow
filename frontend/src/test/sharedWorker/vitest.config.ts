// The fixture files of sharedWorkerRunner.test.ts, in the `unit` project's
// setting, each run in its order (fixtureOrder.ts). They are not test
// files, so no other run collects them.
import { resolve } from 'node:path';
import { defineConfig } from 'vitest/config';
import { BaseSequencer, type TestSpecification } from 'vitest/node';
import { unitProject } from '../../../vitest.config.ts';
import { FIXTURE_ORDER } from './fixtureOrder.ts';

const position = (spec: TestSpecification): number => {
  const index = FIXTURE_ORDER.findIndex((name) => spec.moduleId.endsWith(`/${name}`));
  if (index < 0) throw new Error(`${spec.moduleId} is not in fixtureOrder.ts`);
  return index;
};

class FixtureOrder extends BaseSequencer {
  override async sort(files: TestSpecification[]): Promise<TestSpecification[]> {
    return [...files].sort((a, b) => position(a) - position(b));
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
    // first run fixture holds one of them.
    pool: 'threads',
    maxWorkers: 2,
    sequence: { sequencer: FixtureOrder },
  },
});
