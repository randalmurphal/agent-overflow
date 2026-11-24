// Runs the fixture files of sharedWorkerRunner.test.ts (vitest.config.ts
// here) in a child process, through runNodeWorkload, three ways in turn:
// the run fixtures, the retirement failures with the default reporter, and
// the collect fixtures collected without a run. Returns what each file and
// test ended as.
// oxlint-disable-next-line no-restricted-imports -- the run's own temporary marker file, not a repository file
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Writable } from 'node:stream';
import { fileURLToPath } from 'node:url';
import { nodeWorkload } from '../helpers/nodeWorkload';
import { COLLECT_FIXTURES, RETIREMENT_FAILURES, RUN_FIXTURES } from './fixtureOrder';
import { FIXTURES_DONE_ENV } from './fixtureState';

export type FixtureFile = {
  readonly name: string;
  readonly state: string | undefined;
  readonly errors: string[];
  readonly tests: Array<{ readonly name: string; readonly state: string | undefined; readonly errors: string[] }>;
};

export type FixtureRuns = {
  readonly run: FixtureFile[];
  /** The retirement failures' run: its exit code, what the default
   * reporter printed, and each file's state as its module ended. */
  readonly failures: {
    readonly exitCode: number | string | null | undefined;
    readonly output: string;
    readonly moduleEnds: ReadonlyArray<{ readonly name: string; readonly state: string }>;
  };
  readonly collected: FixtureFile[];
};

type Task = { name: string; type: string; tasks?: Task[]; result?: { state?: string; errors?: Array<{ message?: string }> } };

const messages = (task: Task): string[] => (task.result?.errors ?? []).map((error) => error.message ?? '');

function tests(task: Task): Task[] {
  return (task.tasks ?? []).flatMap((child) => (child.type === 'test' ? [child] : tests(child)));
}

function summarize(files: Task[]): FixtureFile[] {
  return files.map((file) => ({
    name: file.name.split('/').at(-1) ?? file.name,
    state: file.result?.state,
    errors: messages(file),
    tests: tests(file).map((test) => ({ name: test.name, state: test.result?.state, errors: messages(test) })),
  }));
}

// Each name as a filter that matches that file alone.
const only = (names: readonly string[]): string[] => names.map((name) => `/${name}`);

export async function runSharedWorkerFixtures(): Promise<string> {
  // Here, not at module scope: the parent test imports this module through
  // Vite, which rewrites a relative URL beside import.meta.url.
  const CONFIG = fileURLToPath(new URL('./vitest.config.ts', import.meta.url));
  const { createVitest, startVitest } = await import('vitest/node');
  const done = mkdtempSync(join(tmpdir(), 'ao-shared-worker-fixtures-'));
  let runFiles: FixtureFile[];
  try {
    const env = { [FIXTURES_DONE_ENV]: join(done, 'done') };
    const run = await startVitest('test', only(RUN_FIXTURES), { config: CONFIG, watch: false, reporters: [], env });
    runFiles = summarize(run.state.getFiles() as unknown as Task[]);
    await run.close();
  } finally {
    rmSync(done, { recursive: true, force: true });
  }

  let output = '';
  const sink = new Writable({
    write(chunk, _encoding, done) {
      output += String(chunk);
      done();
    },
  });
  // A reporter that counts a file as its module ends sees the first result
  // published for it.
  const moduleEnds: Array<{ name: string; state: string }> = [];
  const atModuleEnd = {
    onTestModuleEnd(module: { moduleId: string; state(): string }) {
      moduleEnds.push({ name: module.moduleId.split('/').at(-1) ?? module.moduleId, state: module.state() });
    },
  };
  process.exitCode = 0;
  const failing = await startVitest('test', only(RETIREMENT_FAILURES), { config: CONFIG, watch: false, reporters: ['default', atModuleEnd] }, {}, { stdout: sink, stderr: sink });
  await failing.close();
  const exitCode = process.exitCode;

  // One worker collects both files, in one request, running them as
  // `vitest list --static-parse=false` and an editor's explorer do.
  const collecting = await createVitest('test', { config: CONFIG, watch: false, maxWorkers: 1 });
  await collecting.collect(only(COLLECT_FIXTURES), { staticParse: false });
  const collected = summarize(collecting.state.getFiles() as unknown as Task[]);
  await collecting.close();

  const runs: FixtureRuns = { run: runFiles, failures: { exitCode, output, moduleEnds }, collected };
  // The fixtures fail on purpose; the parent reads the result, not the code.
  process.exitCode = 0;
  return JSON.stringify(runs);
}

export const sharedWorkerFixturesWorkload = nodeWorkload(import.meta.url, { runSharedWorkerFixtures });
