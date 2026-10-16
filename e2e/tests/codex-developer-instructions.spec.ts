// Developer instructions on a resumed Codex thread.
//
// A resumed Codex thread restores the model's context from its rollout and
// never shows a changed `developerInstructions` to the model, so the app
// injects the changed text with `thread/inject_items` before the session's
// first turn, once per change. Coverage: a fresh thread/start carries the
// app guide and injects nothing; the guide switched off between two
// sessions of the thread reaches the resumed session as one developer
// message without it; the next session, unchanged, injects nothing. Each
// session is its own mock process, so its latched `injectedItems` is that
// session's alone.
import { test, expect, type SeedResult } from './fixtures.js';
import type { MockInfo } from './workflows-helpers.js';

const APP_GUIDE_OPENING = 'You are running inside Agent Overflow';
const UPDATE_OPENING = 'The developer instructions for this conversation have changed.';

type Harness = { rpc: <T>(method: string, ...args: unknown[]) => Promise<T> };

async function codexMocks(harness: Harness): Promise<MockInfo[]> {
  const mocks = await harness.rpc<MockInfo[]>('HarnessListMocks');
  return mocks.filter((mock) => mock.registration.protocol === 'codex');
}

// Starts the thread's session and returns the one Codex mock it spawned.
// StartSession returns after the session's thread is loaded, and the mock
// reports an injection before it answers it, so the latched items are
// complete when this returns.
async function startSession(harness: Harness, threadId: string, known: Set<string>): Promise<MockInfo> {
  await harness.rpc('StartSession', threadId);
  const spawned = (await codexMocks(harness)).filter((mock) => !known.has(mock.mockId));
  expect(spawned).toHaveLength(1);
  known.add(spawned[0].mockId);
  return spawned[0];
}

test('a guide change between two sessions of a Codex thread reaches the resumed thread once', async ({
  harness,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'codex-instructions', repo: {}, threads: [{ title: 'Instructions', provider: 'codex' }] }],
  });
  const [threadId] = seed.projects[0].threadIds;
  const known = new Set((await codexMocks(harness)).map((mock) => mock.mockId));

  try {
    const first = await startSession(harness, threadId, known);
    expect(first.sessionConfig?.developerInstructions).toContain(APP_GUIDE_OPENING);
    expect(first.injectedItems ?? []).toEqual([]);
    await expect
      .poll(async () => (await harness.rpc<{ sessionRef?: string }>('GetThread', threadId)).sessionRef ?? '')
      .not.toBe('');
    await harness.rpc('StopSession', threadId);

    await harness.rpc('UpdateSettings', { agentGuideEnabled: false });
    const resumed = await startSession(harness, threadId, known);
    const injected = resumed.injectedItems ?? [];
    expect(injected).toHaveLength(1);
    expect(injected[0].role).toBe('developer');
    expect(injected[0].text.startsWith(UPDATE_OPENING)).toBe(true);
    expect(injected[0].text).not.toContain(APP_GUIDE_OPENING);
    await harness.rpc('StopSession', threadId);

    const unchanged = await startSession(harness, threadId, known);
    expect(unchanged.injectedItems ?? []).toEqual([]);
  } finally {
    await harness.rpc('StopSession', threadId);
    await harness.rpc('UpdateSettings', { agentGuideEnabled: true });
  }
});
