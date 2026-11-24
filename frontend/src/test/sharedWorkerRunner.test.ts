// One child process runs the fixture files in src/test/sharedWorker/ in the
// `unit` project's setting (sharedWorker/fixtureOrder.ts has the lists):
//   - the run fixtures, in a fixed order, in one worker that takes one run
//     request per file, as each worker of a run with more than one does;
//     each leaves something behind, and the file after it checks that
//     nothing of it is active and that its own listener and timer work;
//   - two files that fail only at their retirement, one that ran and one
//     whose tests are all skipped, with the default reporter;
//   - two files collected without a run, in one worker.
//
// This pins the Vitest behavior sharedWorkerRunner.ts relies on: that
// `onAfterRunSuite` runs for a file that ran before its result is
// published, that `onBeforeRunSuite` does for a file whose tests are all
// skipped, that `onBeforeCollect` runs before each file a worker collects
// (which retires a file that failed while loading), and that the result
// the first two leave is the one the reporter counts and the exit code
// reflects. A Vitest upgrade that changes any of
// them fails here. About 6 s, most of it the three Vitest starts.
import { beforeAll, expect, it } from 'vitest';
import { runNodeWorkload } from './helpers/nodeWorkload';
import { FIXTURE_ORDER } from './sharedWorker/fixtureOrder';
import { sharedWorkerFixturesWorkload, type FixtureFile, type FixtureRuns } from './sharedWorker/fixtures.workload';

// Every file the child loads, read as text so `vitest --changed` runs this
// file when a fixture changes; the child's config names them as strings.
const FIXTURE_SOURCES = import.meta.glob('./sharedWorker/*.ts', { query: '?raw', import: 'default', eager: true });

let runs: FixtureRuns;

beforeAll(() => {
  runs = JSON.parse(runNodeWorkload(sharedWorkerFixturesWorkload, {})) as FixtureRuns;
}, 130_000);

function file(files: readonly FixtureFile[], name: string): FixtureFile {
  const found = files.find((candidate) => candidate.name === name);
  if (found === undefined) throw new Error(`the child ran no ${name}: ${JSON.stringify(files.map((f) => f.name))}`);
  return found;
}

const ran = (name: string) => file(runs.run, name);

function expectPassed(name: string): void {
  const { state, tests } = ran(name);
  expect({ state, tests }, name).toEqual({ state: 'pass', tests: tests.map((test) => ({ ...test, state: 'pass', errors: [] })) });
}

function expectFailedAfterPassing(name: string): string {
  const { state, tests, errors } = ran(name);
  expect(tests.map((test) => test.state), name).toEqual(['pass']);
  expect(state, name).toBe('fail');
  return errors.join('\n');
}

it('runs every fixture file in the directory', () => {
  const fixtures = Object.keys(FIXTURE_SOURCES).filter((path) => path.endsWith('.fixture.ts')).map((path) => path.split('/').at(-1));
  expect(fixtures.sort()).toEqual([...FIXTURE_ORDER].sort());
});

it('runs every other run fixture in one worker while the first holds the other', () => {
  expectPassed('holdsWorker.fixture.ts');
  expectPassed('releasesWorker.fixture.ts');
});

it('fails a file that leaves a global changed, after its tests passed', () => {
  expect(expectFailedAfterPassing('leavesGlobal.fixture.ts')).toContain('leavesGlobal.fixture.ts left shared globals changed for the next file in its worker');
});

it("charges what a file's document teardown installs and imports to that file, and releases it with the file", () => {
  expectPassed('leavesElement.fixture.ts');
  expectPassed('afterLeavesElement.fixture.ts');
});

it('fails the next file naming a listener installed after a file retired, and releases it at once', () => {
  expectPassed('leavesImmediate.fixture.ts');
  expect(expectFailedAfterPassing('afterLeavesImmediate.fixture.ts')).toMatch(
    /^after src\/test\/sharedWorker\/leavesImmediate\.fixture\.ts retired and before src\/test\/sharedWorker\/afterLeavesImmediate\.fixture\.ts started, code installed a resource, released at once:\n {2}a 'ao-fixture' listener, installed at\n.*fixtureState\.ts.*\n.*leavesImmediate\.fixture\.ts/,
  );
  expectPassed('afterTripwire.fixture.ts');
});

it('retires a file whose tests are all skipped before the next file starts', () => {
  expect(ran('skipped.fixture.ts').state).toBe('skip');
  expectPassed('afterSkipped.fixture.ts');
});

it('retires a file that fails while loading before the next file starts', () => {
  expect(ran('failsLoading.fixture.ts').errors).toEqual(['fixture fails while loading']);
  expectPassed('afterFailsLoading.fixture.ts');
});

it('waits for the imports a file started, then releases what they installed and re-evaluates every module', () => {
  expectPassed('lateImport.fixture.ts');
  expectPassed('afterLateImport.fixture.ts');
});

it('fails a file whose import rejects after its last test, naming the import and the rejection', () => {
  expect(expectFailedAfterPassing('rejectsImport.fixture.ts')).toBe(
    'src/test/sharedWorker/rejectsImport.fixture.ts started a dynamic import that rejected after its last test: ' +
      'src/test/sharedWorker/rejectsImport.fixture.ts: import("./rejectingModule"): the rejecting module failed after the last test. ' +
      'Await the import in the test that starts it.',
  );
});

it('publishes a retirement failure as the first result of a file that ran, so a reporter counting at module end counts a failure', () => {
  expect([...runs.failures.moduleEnds].sort((a, b) => a.name.localeCompare(b.name))).toEqual([
    { name: 'leavesGlobal.fixture.ts', state: 'failed' },
    // Vitest reports a module's state from the mode it collected the file
    // in; the default reporter counts the file failed as well.
    { name: 'skippedLeavesGlobal.fixture.ts', state: 'skipped' },
  ]);
});

it("reports a file that fails only at its retirement as failed, in the default reporter's output and the exit code", () => {
  const { exitCode } = runs.failures;
  // eslint-disable-next-line no-control-regex -- the reporter's colors
  const output = runs.failures.output.replace(/\u001b\[[0-9;]*m/g, '');
  expect(exitCode).toBe(1);
  expect(output).toMatch(/Test Files {2}2 failed \| 1 skipped \(2\)/);
  for (const name of ['leavesGlobal', 'skippedLeavesGlobal']) {
    expect(output).toMatch(new RegExp(`FAIL .*src/test/sharedWorker/${name}\\.fixture\\.ts`));
    expect(output).toContain(`src/test/sharedWorker/${name}.fixture.ts left shared globals changed for the next file in its worker`);
  }
});

it('retires a file collected without a run before the next file is collected', () => {
  expect(runs.collected.map(({ name, errors }) => ({ name, errors }))).toEqual(expect.arrayContaining([
    { name: 'collectsListener.fixture.ts', errors: [] },
    { name: 'afterCollectsListener.fixture.ts', errors: [] },
  ]));
  expect(runs.collected).toHaveLength(2);
});
