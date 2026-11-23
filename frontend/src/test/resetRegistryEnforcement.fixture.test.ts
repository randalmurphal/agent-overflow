import { afterAll, describe, it, onTestFinished } from 'vitest';
import { GitCommit, GitPull, GitPush } from './mocks/bindings-app';

// The file resetRegistryEnforcement.test.ts runs in a child Vitest. Each
// case makes a binding call with no mock that the code under test catches,
// at one of the three points the registry checks; the child run fails at
// each. Skipped in every other run.
function swallow(call: () => unknown): void {
  try {
    void Promise.resolve(call()).catch(() => {});
  } catch {
    // Caught, as the code under test would; the registry still saw it.
  }
}

describe.runIf(process.env.AO_REGISTRY_ENFORCEMENT_FIXTURE === '1')('unanswered calls', () => {
  it('calls GitCommit during the test', () => {
    swallow(() => GitCommit('/w', '', [], '', ''));
  });

  it('calls GitPush after the test ended', () => {
    onTestFinished(() => swallow(() => GitPush('/w')));
  });

  it('runs after the test that called GitPush', () => {});

  afterAll(() => swallow(() => GitPull('/w')));
});
