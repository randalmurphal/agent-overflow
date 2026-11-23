import { expect, it, onTestFinished } from 'vitest';
import { resetRegistry } from './resetRegistry';
import { GitCommit, GitPush, setBindingMock } from './mocks/bindings-app';
import { Call } from './mocks/wailsio-runtime';
import { methodId } from '../lib/transport/methodId';

// Whom the registry charges a binding call with no mock to. Each test here
// takes the calls it records, so the checks pass; resetRegistryEnforcement
// shows the checks failing a run.
const FILE = 'src/test/resetRegistryUnanswered.test.ts';

function swallow(call: () => unknown): void {
  try {
    void Promise.resolve(call()).catch(() => {});
  } catch {
    // The code under test catches the failure; the record still holds it.
  }
}

it('charges an unmocked call to the test that made it, though the caller caught it', async () => {
  swallow(() => GitCommit('/w', '', [], '', ''));
  swallow(() => Call.ByName('main.App.GitPush'));
  swallow(() => Call.ByID(methodId('ArchiveThread'), 't1'));
  const during = `in "charges an unmocked call to the test that made it, though the caller caught it" (${FILE})`;
  expect(resetRegistry.takeUnansweredBindings()).toEqual([
    `GitCommit, ${during}`,
    `GitPush, ${during}`,
    `ArchiveThread, ${during}`,
  ]);
  expect(resetRegistry.takeUnansweredBindings()).toEqual([]);
});

let afterEnd: string[] = [];

it('charges a call made after a test ended to that test', () => {
  // Runs after the registry's after-test steps and checks.
  onTestFinished(() => {
    swallow(() => GitPush('/w'));
    afterEnd = resetRegistry.takeUnansweredBindings();
  });
});

it('names the ended test for the call made after it', () => {
  expect(afterEnd).toEqual([`GitPush, after "charges a call made after a test ended to that test" (${FILE}) ended`]);
});

it('records nothing for a binding a test makes absent with a rejecting mock', async () => {
  setBindingMock('GitPush', async () => {
    throw new Error('absent');
  });
  await expect(GitPush('/w')).rejects.toThrow('absent');
  expect(resetRegistry.takeUnansweredBindings()).toEqual([]);
});
