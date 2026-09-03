// Edit-and-resend through the REAL UI and the real backend saga: the
// pencil on a past user message, the in-place editor, and the one RPC
// that reverts the conversation and sends the replacement under a
// single thread lock.
//
// What only this level can prove is the CHOREOGRAPHY. The backend emits
// `user_message:reverted` before it dispatches the resend, and both
// frames travel the same FIFO WebSocket, so the user must see the tail
// collapse and THEN the replacement arrive — never a replacement row
// landing in a timeline that is about to be cut. The `step-gated`
// scenario is what makes that observable instead of racy: the resend's
// mock session parks before it emits a single assistant frame, so the
// truncated-and-resent state is a stable thing to assert on rather than
// a moment to catch.
//
// Scope note: the anchor here is the thread's FIRST user message, whose
// rollback drops the Claude session reference outright. Reverting to a
// LATER message slices the provider's session JSONL, and the mock
// provider writes no session file — see the spec's tail comment.
import { test, expect, type HarnessMockEvent, type SeedResult } from './fixtures.js';

const EDITED = 'How do I sort an array of objects by a key?';

async function seedTwoTurnThread(harness: {
  rpc: <T>(method: string, ...args: unknown[]) => Promise<T>;
}): Promise<string> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'edit-resend-app',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [
          {
            title: 'Sorting question',
            turns: [
              {
                userText: 'How do I sort an array in JS?',
                items: [{ kind: 'assistant_text', summary: 'Use Array.prototype.sort.' }],
              },
              {
                userText: 'And in reverse?',
                items: [{ kind: 'assistant_text', summary: 'Reverse the comparator.' }],
              },
            ],
          },
        ],
      },
    ],
  });
  return seed.projects[0].threadIds[0];
}

test('editing a past user message truncates the tail before the replacement streams', async ({
  harness,
  page,
}) => {
  // Set before anything can spawn: the resend starts a NEW mock session
  // (the revert stops the old one), and a scenario only reaches mocks
  // that register after it is set.
  await harness.rpc('HarnessSetScenario', { name: 'step-gated' });
  await seedTwoTurnThread(harness);

  await harness.open(page);
  await page.getByText('Sorting question').click();
  await expect(page.getByText('How do I sort an array in JS?')).toBeVisible();
  await expect(page.getByText('Use Array.prototype.sort.')).toBeVisible();
  await expect(page.getByText('And in reverse?')).toBeVisible();
  await expect(page.getByText('Reverse the comparator.')).toBeVisible();

  // Open the editor on the FIRST user message.
  const firstUserRow = page.getByText('How do I sort an array in JS?');
  await firstUserRow.hover();
  await page.getByLabel('Edit message and resend from here').first().click();
  const editor = page.getByTestId('user-message-editor');
  await expect(editor).toBeVisible();

  // Nothing is destroyed by opening the editor: the timeline truncates
  // only when the backend's own event lands.
  await expect(page.getByText('Reverse the comparator.')).toBeVisible();
  await expect(page.getByText('And in reverse?')).toBeVisible();

  await editor.getByLabel('Message Input').fill(EDITED);
  await page.getByTestId('user-message-edit-send').click();

  // The mock for the resent turn parks at its first gate, so this is a
  // STABLE state, not a frame we raced: the revert has committed, the
  // replacement user row has landed, and zero assistant frames of the
  // new turn have streamed.
  const registered = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'registered',
  );
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'waiting_signal' && ev.report.detail === 'block-start',
  );

  await expect(page.getByText(EDITED)).toBeVisible();
  await expect(page.getByText('How do I sort an array in JS?')).toHaveCount(0);
  await expect(page.getByText('Use Array.prototype.sort.')).toHaveCount(0);
  await expect(page.getByText('And in reverse?')).toHaveCount(0);
  await expect(page.getByText('Reverse the comparator.')).toHaveCount(0);
  await expect(page.getByText('First chunk.')).toHaveCount(0);
  // The editor is gone with the row it was attached to.
  await expect(page.getByTestId('user-message-editor')).toHaveCount(0);

  // The new turn streams onto the truncated timeline like any other.
  const advance = (name: string) =>
    harness.rpc('HarnessMockCommand', registered.mockId, { type: 'advance', name });
  await advance('block-start');
  await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'waiting_signal' && ev.report.detail === 'first-delta',
  );
  await advance('first-delta');
  await expect(page.getByText('First chunk.')).toBeVisible();
  await advance('second-delta');
  await advance('finish');
  await harness.waitForEvent('provider:turn_completed');
  await expect(page.getByText('First chunk. Second chunk.')).toBeVisible();
  await expect(page.getByText(EDITED)).toBeVisible();
});

test('the edited text reaches the provider, not the original', async ({ harness, page }) => {
  await seedTwoTurnThread(harness);

  await harness.open(page);
  await page.getByText('Sorting question').click();
  await page.getByText('How do I sort an array in JS?').hover();
  await page.getByLabel('Edit message and resend from here').first().click();
  const editor = page.getByTestId('user-message-editor');
  await editor.getByLabel('Message Input').fill(EDITED);
  await page.getByTestId('user-message-edit-send').click();

  // The mock reports the text it actually read off the wire — the one
  // place the whole edit path is proved end to end rather than at the
  // timeline's word.
  const received = await harness.waitForEvent<HarnessMockEvent>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input',
  );
  expect(received.report.input).toContain(EDITED);
  expect(received.report.input).not.toContain('How do I sort an array in JS?');
});

test('cancelling the editor leaves the conversation exactly as it was', async ({
  harness,
  page,
}) => {
  await seedTwoTurnThread(harness);

  await harness.open(page);
  await page.getByText('Sorting question').click();
  await page.getByText('How do I sort an array in JS?').hover();
  await page.getByLabel('Edit message and resend from here').first().click();
  const editor = page.getByTestId('user-message-editor');
  await editor.getByLabel('Message Input').fill(EDITED);

  await page.getByTestId('user-message-edit-cancel').click();
  await page.getByRole('button', { name: 'Discard' }).click();

  await expect(page.getByTestId('user-message-editor')).toHaveCount(0);
  await expect(page.getByText('How do I sort an array in JS?')).toBeVisible();
  await expect(page.getByText('Use Array.prototype.sort.')).toBeVisible();
  await expect(page.getByText('And in reverse?')).toBeVisible();
  await expect(page.getByText('Reverse the comparator.')).toBeVisible();
  await expect(page.getByText(EDITED)).toHaveCount(0);
});

// Not covered here, deliberately: reverting to a LATER user message, whose
// rollback slices the provider's session JSONL instead of dropping the
// session reference outright. The harness cannot express it — its mock
// provider writes no session file, so there is nothing to slice and the
// branch would pass for the wrong reason. `TestRevertAndResendReplacesMessageAndRestoresWIP`
// (app_revert_and_resend_test.go) is the substituting coverage: it runs
// the same saga against real store rows and asserts the post-revert
// conversation and the restored composer draft directly.
