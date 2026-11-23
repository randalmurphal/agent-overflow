// One child Vitest runs the fixture files in src/test/sharedWorker/ in the
// `unit` project's setting, in a fixed order, in one worker that takes one
// run request per file, as each worker of a run with more than one does
// (sharedWorker/vitest.config.ts has the order). Each fixture leaves
// something behind, and the file after it asserts that nothing of it is
// active and that its own listener and timer work.
//
// This pins the Vitest behavior sharedWorkerRunner.ts relies on: that
// `onAfterRunFiles` runs for a file whose tests are all skipped and for a
// file that fails while loading, and that a file result sent again after
// it reaches the run's state. A Vitest upgrade that changes either fails
// here. About 4 s, most of it the child's start.
import { beforeAll, expect, it } from 'vitest';
import { runNodeWorkload } from './helpers/nodeWorkload';
import { sharedWorkerFixturesWorkload, type FixtureFile, type FixtureRun } from './sharedWorker/fixtures.workload';
// Imported so `vitest --changed` runs this file when the runner changes.
import SharedWorkerRunner from './sharedWorkerRunner';

let run: FixtureRun;

beforeAll(() => {
  expect(SharedWorkerRunner).toBeTypeOf('function');
  run = JSON.parse(runNodeWorkload(sharedWorkerFixturesWorkload, {})) as FixtureRun;
}, 130_000);

function file(name: string): FixtureFile {
  const found = run.files.find((candidate) => candidate.name === name);
  if (found === undefined) throw new Error(`the child ran no ${name}: ${JSON.stringify(run.files.map((f) => f.name))}`);
  return found;
}

function expectPassed(name: string): void {
  const { state, tests } = file(name);
  expect({ state, tests }, name).toEqual({ state: 'pass', tests: tests.map((test) => ({ ...test, state: 'pass', errors: [] })) });
}

it('runs every other fixture in one worker while the first holds the other', () => {
  expectPassed('holdsWorker.fixture.ts');
});

it('fails a file that leaves a global changed, after its tests passed, in the run result and exit code', () => {
  const leaves = file('leavesGlobal.fixture.ts');
  expect(leaves.tests.map((test) => test.state)).toEqual(['pass']);
  expect(leaves.state).toBe('fail');
  expect(leaves.errors.join('\n')).toContain('leavesGlobal.fixture.ts left shared globals changed for the next file in its worker');
  expect(run.exitCode).toBe(1);
});

it('fails the next file naming a listener installed while no file was open, and releases it at once', () => {
  expectPassed('leavesElement.fixture.ts');
  const next = file('afterLeavesElement.fixture.ts');
  expect(next.tests.map((test) => test.state)).toEqual(['pass']);
  expect(next.state).toBe('fail');
  expect(next.errors.join('\n')).toMatch(
    /^after src\/test\/sharedWorker\/leavesElement\.fixture\.ts retired and before src\/test\/sharedWorker\/afterLeavesElement\.fixture\.ts started, code installed a resource, released at once:\n {2}a 'ao-fixture' listener, installed at\n.*fixtureState\.ts.*\n.*leavesElement\.fixture\.ts/,
  );
  expectPassed('afterTripwire.fixture.ts');
});

it('retires a file whose tests are all skipped before the next file starts', () => {
  expect(file('skipped.fixture.ts').state).toBe('skip');
  expectPassed('afterSkipped.fixture.ts');
});

it('retires a file that fails while loading before the next file starts', () => {
  expect(file('failsLoading.fixture.ts').errors).toEqual(['fixture fails while loading']);
  expectPassed('afterFailsLoading.fixture.ts');
});

it('waits for the imports a file started, then releases what they installed and re-evaluates every module', () => {
  expectPassed('lateImport.fixture.ts');
  expectPassed('afterLateImport.fixture.ts');
});
