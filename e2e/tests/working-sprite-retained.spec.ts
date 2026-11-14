// The working sprite paints from memory on every turn, not from a fresh
// backend fetch. The backend serves /assets/* to a loopback page as
// no-store, so a strip whose image the renderer has collected between
// turns is refetched, and the chip shows an empty slot until the backend
// answers (seconds under load). lib/spinners/retain.ts holds every
// showable strip from the moment animations are on; this proves the
// strip is fetched once across two turns with a garbage collection
// between them. Real backend headers and real Chromium on purpose: the
// unit suites cannot see HTTP caching or image resource lifetime.
import { readdirSync } from 'node:fs';
import { test, expect, type HarnessMockEvent, type SeedResult } from './fixtures.js';

const SPRITE = 'party-parrot';
// One sprite in the pool, so both turns show the same strip: every other
// bundled strip (frontend/src/assets/spinners/<id>.png) is unchecked.
const OTHER_SPRITE_IDS = readdirSync(new URL('../../frontend/src/assets/spinners/', import.meta.url))
  .filter((name) => name.endsWith('.png'))
  .map((name) => name.slice(0, -'.png'.length))
  .filter((id) => id !== SPRITE);

const scenario = {
  version: 1,
  name: 'working-sprite-retained',
  provider: 'claude',
  turns: [
    {
      label: 'held-turn',
      steps: [
        {
          emit: {
            lines: [
              '{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg-${TURN}","role":"assistant"}}}',
            ],
          },
        },
        { waitSignal: { name: 'finish' } },
        {
          emit: {
            lines: [
              '{"type":"assistant","message":{"id":"msg-${TURN}","role":"assistant","content":[{"type":"text","text":"Turn ${TURN} done."}]}}',
              '{"type":"result","subtype":"success","is_error":false}',
            ],
          },
        },
      ],
    },
  ],
  afterTurns: 'repeatLast',
};

test('the working sprite strip is fetched once across turns', async ({ harness, page, context }) => {
  await harness.rpc('UpdateSettings', {
    spinnerAnimationsEnabled: true,
    spinnerDisabledAnimations: OTHER_SPRITE_IDS,
    spinnerCompactionAnimation: 'none',
  });
  await harness.rpc('HarnessSetScenario', { scenario });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'sprite-app', repo: {}, threads: [{ title: 'Sprite turns' }] }],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.rpc('StartSession', threadId);
  const registered = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'registered',
  );

  const finishGate = () => harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'waiting_signal' && ev.report.detail === 'finish',
  );
  // An empty thread is not listed, so turn 1 is under way before the page
  // opens; the app's boot and the rail then race for the same strip.
  let gate = finishGate();
  await harness.rpc('SendMessage', threadId, 'turn 1', null);
  await gate;

  const stripFetches: string[] = [];
  page.on('response', (response) => {
    if (new URL(response.url()).pathname.includes(`/assets/${SPRITE}-`)) {
      stripFetches.push(response.headers()['cache-control'] ?? '');
    }
  });
  await harness.open(page);
  await page.getByText('Sprite turns').click();

  const sprite = page.locator('[data-testid="activity-rail-sprite"] .working-sprite');
  const cdp = await context.newCDPSession(page);
  for (const turn of [1, 2]) {
    if (turn > 1) {
      gate = finishGate();
      await harness.rpc('SendMessage', threadId, `turn ${turn}`, null);
      await gate;
    }
    await expect(sprite).toHaveAttribute('data-sprite-id', SPRITE);
    const completed = harness.waitForEvent('provider:turn_completed');
    await harness.rpc('HarnessMockCommand', registered.mockId, { type: 'advance', name: 'finish' });
    await completed;
    await expect(sprite).toHaveCount(0);
    // Collect the finished turn's strip, as the renderer eventually does
    // on its own between turns.
    await cdp.send('HeapProfiler.collectGarbage');
    await cdp.send('HeapProfiler.collectGarbage');
  }

  // The premise: without no-store a second fetch would be a cache hit
  // and this spec would prove nothing about retention.
  expect(stripFetches).toEqual(['no-store']);
  await cdp.detach();
});
