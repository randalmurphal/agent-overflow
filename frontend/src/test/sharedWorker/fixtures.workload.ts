// Runs the fixture files of sharedWorkerRunner.test.ts (vitest.config.ts
// here) in a child Vitest, through runNodeWorkload, and returns what each
// file and test ended as.
import type { NodeWorkload } from '../helpers/nodeWorkload';

export type FixtureFile = {
  readonly name: string;
  readonly state: string | undefined;
  readonly errors: string[];
  readonly tests: Array<{ readonly name: string; readonly state: string | undefined; readonly errors: string[] }>;
};

export type FixtureRun = { readonly files: FixtureFile[]; readonly exitCode: number | string | null | undefined };

type Task = { name: string; type: string; tasks?: Task[]; result?: { state?: string; errors?: Array<{ message?: string }> } };

const messages = (task: Task): string[] => (task.result?.errors ?? []).map((error) => error.message ?? '');

function tests(task: Task): Task[] {
  return (task.tasks ?? []).flatMap((child) => (child.type === 'test' ? [child] : tests(child)));
}

export async function runSharedWorkerFixtures(): Promise<string> {
  const { startVitest } = await import('vitest/node');
  const vitest = await startVitest('test', [], {
    config: new URL('./vitest.config.ts', import.meta.url).pathname,
    watch: false,
    reporters: [],
  });
  const files = (vitest.state.getFiles() as unknown as Task[]).map((file) => ({
    name: file.name.split('/').at(-1) ?? file.name,
    state: file.result?.state,
    errors: messages(file),
    tests: tests(file).map((test) => ({ name: test.name, state: test.result?.state, errors: messages(test) })),
  }));
  await vitest.close();
  const run: FixtureRun = { files, exitCode: process.exitCode };
  // The fixtures fail on purpose; the parent reads the result, not the code.
  process.exitCode = 0;
  return JSON.stringify(run);
}

export const sharedWorkerFixturesWorkload: NodeWorkload = { url: import.meta.url, entry: 'runSharedWorkerFixtures' };
