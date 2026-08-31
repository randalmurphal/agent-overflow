// Regression (field bug 2026-08-22): the wails-generated Settings model
// class materializes wire-omitted optional keys as own undefined
// properties, and the frontend defaults merge spread copied them over
// the defaults — so the untouched compaction slot reached the sprite
// resolver as undefined and compaction showed a random pool sprite.
// Full pipeline on purpose: real GetSettings wire, real triage
// compacting state, real SPA. The unit suites mock bindings with plain
// objects, which genuinely omit keys, and cannot see this class of bug.
import { test, expect, type HarnessMockEvent, type SeedResult } from './fixtures.js';

const scenario = {
  version: 1,
  name: 'compacting-stall',
  provider: 'claude',
  turns: [
    {
      label: 'compact-midturn',
      steps: [
        {
          emit: {
            delayBetweenMs: 30,
            lines: [
              '{"type":"stream_event","event":"message_start","data":{"type":"message_start","message":{"id":"msg-1","role":"assistant"}}}',
              '{"type":"stream_event","event":"content_block_start","data":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}',
              '{"type":"stream_event","event":"content_block_delta","data":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Working on it..."}}}',
              '{"type":"system","subtype":"status","status":"compacting"}',
            ],
          },
        },
      ],
    },
  ],
  afterTurns: 'silent',
};

test('compaction shows the default compaction sprite with field settings', async ({
  harness,
  page,
}) => {
  await harness.rpc('UpdateSettings', {
    spinnerAnimationsEnabled: true,
    spinnerDisabledAnimations: [
      'nyan-cat',
      'party-parrot-classic',
      'robo-jam',
      'robo-marathon',
      'robo-papers',
    ],
  });
  await harness.rpc('HarnessSetScenario', { scenario });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'compact-app', repo: {}, threads: [{ title: 'Compact turn' }] }],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.rpc('StartSession', threadId);
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'registered',
  );
  await harness.rpc('SendMessage', threadId, 'go compact', null);
  await harness.waitForEvent('provider:compacting');

  await harness.open(page);
  await page.getByText('Compact turn').click();
  await expect(page.getByText('Compacting')).toBeVisible();
  const sprite = page.locator('[data-testid="activity-rail-sprite"] .working-sprite');
  await expect(sprite).toBeVisible();
  await expect(sprite).toHaveAttribute('data-sprite-id', 'robo-papers');
});
