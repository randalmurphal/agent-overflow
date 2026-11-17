// ao-thread-tools asked to start work on a paired computer that is off.
//
// Coverage: a spawn on a computer whose backend is stopped is refused by
// its own call, saying nothing was sent, and leaves no open request: the
// caller is not handed a token to watch, and forgetting the computer does
// not name a request to abandon.
// Specs: docs/specs/agent-thread-tools.md ("Request identity and
// durability").
import { test, expect } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import { startTogether } from './launch-helpers.js';
import { awaitToolAnswer, setScenario, threadToolsScenario } from './thread-tools-helpers.js';

interface SeededProject {
  projectId: string;
  path: string;
  threadIds: string[];
}

async function seed(app: HarnessApp, name: string, titles: string[]): Promise<SeededProject> {
  const result = await app.rpc<{ projects: SeededProject[] }>('HarnessSeed', {
    projects: [{ name, repo: {}, threads: titles.map((title) => ({ title, provider: 'claude' })) }],
  });
  return result.projects[0];
}

test('a spawn on a computer that is off is refused at once and leaves nothing open', async () => {
  test.setTimeout(180_000);
  let home: HarnessApp | undefined;
  let remote: HarnessApp | undefined;
  try {
    [home, remote] = await startTogether(launchHarness(), launchHarness());
    const pairing = await headlessPairing(remote);
    let remoteID = '';
    try {
      const attachment = await home.rpc<{ id: string; verificationNumber: string }>('AddBackend', pairing.invite.url);
      await pairing.confirm(attachment.verificationNumber);
      remoteID = attachment.id;
    } finally {
      pairing.close();
    }
    const caller = await seed(home, 'asleep-caller', ['Asleep caller']);
    const there = await seed(remote, 'asleep-work', []);
    await setScenario(
      home,
      caller.path,
      threadToolsScenario({
        name: 'asleep-caller-script',
        provider: 'claude',
        turns: [
          {
            steps: [
              {
                call: {
                  tool: 'thread_spawn',
                  args: {
                    prompt: 'Start something over there.',
                    computer_id: remoteID,
                    project_id: there.projectId,
                    notify: true,
                  },
                  timeoutMs: 120_000,
                },
              },
            ],
            text: 'That computer is off.',
          },
        ],
      }),
    );

    expect(await remote.stop()).toBe(true);
    await home.rpc('StartSession', caller.threadIds[0]);
    await home.rpc('SendMessage', caller.threadIds[0], 'start something over there', null);

    const spawn = await awaitToolAnswer(home, { tool: 'thread_spawn', timeoutMs: 120_000 });
    expect(spawn.isError, spawn.text).toBe(true);
    expect(spawn.text).toContain('nothing was sent');
    expect(spawn.text).not.toContain('thread_status');

    // Nothing is left open there, so forgetting the computer asks about
    // no request.
    await home.rpc('RemoveBackend', remoteID, false);
    expect(await home.rpc<Array<{ id: string }>>('ListBackends')).toHaveLength(0);
  } finally {
    await remote?.close();
    await home?.close();
  }
});
