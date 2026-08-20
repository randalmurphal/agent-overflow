// Composer ArrowUp history recall through the real app: the binding
// (GetThreadUserMessageHistory), the store read, and the caret-gated
// keyboard claim, in a real browser where the native caret movement the
// gate rides on actually happens. What only this level can prove is the
// two-step feel over a real textarea: ArrowUp with the caret mid-text
// first jumps to offset 0 (native), and only the NEXT ArrowUp walks
// history — plus that browsing never rewrites the persisted draft row.
import { test, expect, type SeedResult } from './fixtures.js';

test('ArrowUp walks past messages from caret 0 and ArrowDown restores the typed draft', async ({
  harness,
  page,
}) => {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'recall-app',
        repo: {},
        threads: [
          {
            title: 'Recall thread',
            turns: [
              {
                userText: 'first ask',
                items: [{ kind: 'assistant_text', summary: 'first answer' }],
              },
              {
                userText: 'second ask',
                items: [{ kind: 'assistant_text', summary: 'second answer' }],
              },
            ],
          },
        ],
      },
    ],
  });

  await page.goto(harness.url);
  await page.getByText('Recall thread').click();
  await expect(page.getByText('second answer')).toBeVisible();

  const textarea = page.getByLabel('Message Input');
  await textarea.click();
  await textarea.fill('typed draft');

  // Caret sits at the end after fill. The first ArrowUp is the native
  // jump to the start of the (single) line; only from offset 0 does the
  // next one walk history.
  await textarea.press('ArrowUp');
  await expect(textarea).toHaveValue('typed draft');
  await textarea.press('ArrowUp');
  await expect(textarea).toHaveValue('second ask');

  // An up-paint parks the caret at offset 0, so each further ArrowUp
  // walks on directly — one press per entry, no native two-step.
  await textarea.press('ArrowUp');
  await expect(textarea).toHaveValue('first ask');

  // At the oldest entry the keystroke is swallowed.
  await textarea.press('ArrowUp');
  await textarea.press('ArrowUp');
  await expect(textarea).toHaveValue('first ask');

  // The caret is still at 0, so turning around costs one native jump to
  // the end of the line. From there each down-paint re-parks the caret
  // at the end, so walking DOWN is one press per entry too.
  await textarea.press('ArrowDown');
  await expect(textarea).toHaveValue('first ask');
  await textarea.press('ArrowDown');
  await expect(textarea).toHaveValue('second ask');
  await textarea.press('ArrowDown');
  await expect(textarea).toHaveValue('typed draft');

  // Nothing below the message being typed.
  await textarea.press('ArrowDown');
  await expect(textarea).toHaveValue('typed draft');

  // Browsing never overwrote the durable draft: a reload hydrates the
  // TYPED text, not a browsed entry. Walk up first so the composer is
  // showing a preview at reload time.
  await textarea.press('ArrowUp');
  await textarea.press('ArrowUp');
  await expect(textarea).toHaveValue('second ask');
  await page.reload();
  // The pane layout restores the open thread on its own.
  await expect(page.getByTestId('chat-header-title')).toHaveText('Recall thread');
  await expect(page.getByLabel('Message Input')).toHaveValue('typed draft');
});
