// A published binary and the candidate each act as spawner and worker.
import { test, expect } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { productBinary } from '../src/product-process.js';
import { startTogether } from './launch-helpers.js';
import { pairOwnDevice } from './headless-pairing-helpers.js';
import { awaitToolAnswer, setScenario, threadToolsScenario } from './thread-tools-helpers.js';
import type { SeedResult } from './fixtures.js';

async function seed(app: HarnessApp, name: string) {
  const { projects } = await app.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name, repo: {}, threads: [{ title: name, provider: 'claude' }] }],
  });
  return projects[0];
}

test('a worker finishes for its spawner in both directions with the released thread protocol', async () => {
  const binary = process.env.AO_RELEASE_UPGRADE_PREVIOUS_BIN;
  if (!binary) throw new Error('Set AO_RELEASE_UPGRADE_PREVIOUS_BIN to the v0.0.19 headless binary');
  const [candidate, released] = await startTogether(launchHarness(), launchHarness({
    binary, savedRelease: true, mockProvider: productBinary('ao-mockprovider'), mockForge: '',
    env: { AO_HARNESS_TIMING: 'pairing-probe=500ms,thread-poll=250ms' },
  }));
  try {
    const releasedID = await pairOwnDevice(candidate, released, 'Released computer');
    const candidates = () => released.rpc<Array<{ id: string }>>('ListAgentComputers');
    await expect.poll(async () => (await candidates()).length).toBe(1);
    const [peer] = await candidates();
    for (const [source, target, computer] of [
      [candidate, released, releasedID], [released, candidate, peer.id],
    ] as const) {
      const caller = await seed(source, 'Mixed version caller');
      const work = await seed(target, 'Mixed version work');
      await setScenario(target, work.path, threadToolsScenario({
        name: 'released-worker', provider: 'claude', turns: [{ text: 'The worker finished.' }],
      }));
      await setScenario(source, caller.path, threadToolsScenario({
        name: 'released-spawner', provider: 'claude', turns: [{ steps: [{ call: { tool: 'thread_spawn', args: {
          computer_id: computer, project_id: work.projectId, title: 'Mixed worker', prompt: 'Complete the task.',
          wait_seconds: 60,
        } } }], text: 'Received the answer.' }],
      }));
      // The released binary starts a session only through its own StartSession.
      if (source === released) await source.rpc('StartSession', caller.threadIds[0]);
      else await source.startSession(caller.threadIds[0]);
      await source.send(caller.threadIds[0], 'Run the worker.');
      const result = await awaitToolAnswer<{ state: string }>(source, { tool: 'thread_spawn', cwd: caller.path, timeoutMs: 90_000 });
      expect(result.isError, result.text).toBe(false);
      expect(result.value?.state).toBe('finished');
      expect(result.text).toContain('The worker finished.');
    }
  } finally {
    await Promise.all([released.close(), candidate.close()]);
  }
});
