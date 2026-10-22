// What a loop's tick does to the sidebar (docs/specs/standing-agents.md,
// "Attention"), on both providers. A tick that ends with nothing to say
// leaves the thread's row where it was, read, and with no status. A tick
// that raises thread_attention lifts the row, marks it unread and shows the
// flag's kind and the agent's reason on the row; opening the thread clears
// it the way it clears a completion. A flagged assistant's card stacks the
// flag's line above its state and projects lines without overlap.
import type { Locator, Page } from '@playwright/test';
import { expect, test, type SeedResult } from './fixtures.js';
import { awaitSidebarSettled, wakeLoopNow } from './wake-rows-helpers.js';
import { cardRow, cardShell, rowIn, shellIn } from './sidebar-sections-helpers.js';
import { awaitToolAnswer, awaitTurnCompleted, setScenario, threadToolsScenario } from './thread-tools-helpers.js';

/** What the sidebar shows of one thread row. */
interface RowState {
  /** Its place among every thread row, top first. */
  index: number;
  bold: boolean;
  /** The status dot's label, or null for no dot. */
  status: string | null;
  attention: { kind: string; reason: string } | null;
}

async function rowStates(page: Page, ids: string[]): Promise<Record<string, RowState>> {
  return await page.evaluate((ids) => {
    const rows = [...document.querySelectorAll<HTMLElement>('[data-testid="thread-row"][data-sidebar-thread-id]')];
    const out: Record<string, RowState> = {};
    for (const id of ids) {
      const index = rows.findIndex((row) => row.dataset.sidebarThreadId === id);
      const row = rows[index];
      if (!row) throw new Error(`no sidebar row for ${id}`);
      const title = row.querySelector<HTMLElement>('[data-testid="thread-row-title"]')!;
      const dot = row.querySelector<HTMLElement>('[data-testid="thread-row-status-dot"]');
      const line = row.closest('[data-testid="thread-row-shell"]')?.querySelector<HTMLElement>('[data-testid="thread-row-attention"]');
      out[id] = {
        index,
        bold: Number(getComputedStyle(title).fontWeight) >= 600,
        status: dot?.getAttribute('aria-label') ?? null,
        attention: line
          ? { kind: line.dataset.kind ?? '', reason: line.querySelector('[data-testid="thread-row-attention-reason"]')?.textContent ?? '' }
          : null,
      };
    }
    return out;
  }, ids);
}

for (const provider of ['claude', 'codex'] as const) {
  test(`${provider}: a quiet tick leaves the sidebar as it was, and a flag shows its kind and reason`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        { name: `quiet-tick-${provider}`, repo: {}, threads: [{ title: 'Ticker', provider }, { title: 'Neighbor', provider }] },
        {
          name: `quiet-tick-elsewhere-${provider}`, repo: {},
          threads: [{ title: 'Elsewhere', provider: 'claude', turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'Hi.' }] }] }],
        },
      ],
    });
    const [tickerP, elsewhereP] = seed.projects;
    const [ticker, neighbor] = tickerP.threadIds;
    const elsewhere = elsewhereP.threadIds[0];
    const reason = 'The release notes are ready for you';
    // Both threads run in the project's directory, so both run this
    // scenario from its first turn; only the ticker goes past it.
    await setScenario(harness, tickerP.path, threadToolsScenario({
      name: `quiet-tick-${provider}`,
      provider,
      echoUserMessage: true,
      turns: [
        { text: 'Hello.' },
        {
          steps: [{ call: { tool: 'thread_loop', args: { label: 'tick', note: 'look at the board', every_seconds: 3600 } } }],
          text: 'Watching.',
        },
        { text: 'Nothing new on the board.' },
        { steps: [{ call: { tool: 'thread_attention', args: { kind: 'review', reason } } }], text: 'Flagged.' },
      ],
      afterTurns: 'silent',
    }));

    // The ticker registers its loop, then the neighbor talks last, so it
    // lists above the ticker. The user has read both.
    await harness.rpc('StartSession', ticker);
    await harness.rpc('SendMessage', ticker, 'hi', null);
    await awaitTurnCompleted(harness, ticker);
    await harness.rpc('SendMessage', ticker, 'watch the board', null);
    const loop = await awaitToolAnswer(harness, { tool: 'thread_loop', cwd: tickerP.path, timeoutMs: 60_000 });
    expect(loop.isError, loop.text).toBe(false);
    await awaitTurnCompleted(harness, ticker);
    await harness.rpc('StartSession', neighbor);
    await harness.rpc('SendMessage', neighbor, 'hi', null);
    await awaitTurnCompleted(harness, neighbor);
    await harness.rpc('MarkThreadsRead', [ticker, neighbor]);

    // The page shows another thread, so neither is read by being on screen.
    await harness.open(page);
    await page.getByTestId('thread-row').filter({ hasText: 'Elsewhere' }).first().click();
    await expect(page.getByTestId('message-timeline-scroll')).toBeVisible();
    const before = await rowStates(page, [ticker, neighbor]);
    expect(before[neighbor].index).toBeLessThan(before[ticker].index);
    expect(before[ticker]).toMatchObject({ bold: false, status: null, attention: null });

    // Barrier: the page has applied everything sent before this rename.
    let renames = 0;
    const caughtUp = async () => {
      const title = `Elsewhere ${++renames}`;
      await harness.rpc('RenameThread', elsewhere, title);
      await expect(page.getByTestId('thread-row').filter({ hasText: title })).toHaveCount(1);
    };
    const tick = async () => {
      await wakeLoopNow(harness, ticker, 'tick');
      await harness.waitForEvent<{ threadId?: string; quiet?: boolean }>(
        'provider:turn_completed',
        (data) => data.threadId === ticker && data.quiet === true,
        60_000,
      );
      await caughtUp();
    };

    // ---- A tick with nothing to say changes nothing the sidebar shows.
    await tick();
    expect(await rowStates(page, [ticker, neighbor])).toEqual(before);

    // ---- A tick that flags a review lifts the row and marks it.
    await tick();
    const row = shellIn(page, ticker);
    await expect(row.getByTestId('thread-row-attention')).toHaveAttribute('data-kind', 'review');
    await expect(row.getByTestId('thread-row-attention-reason')).toHaveText(reason);
    const flagged = await rowStates(page, [ticker, neighbor]);
    expect(flagged[ticker]).toEqual({
      index: flagged[ticker].index,
      bold: true,
      status: `Ready for your review: ${reason}`,
      attention: { kind: 'review', reason },
    });
    expect(flagged[ticker].index).toBeLessThan(flagged[neighbor].index);
    expect(flagged[neighbor]).toMatchObject({ bold: false, status: null, attention: null });
    await row.screenshot({ path: test.info().outputPath(`${provider}-attention-row.png`) });

    // ---- Opening the thread reads the flag.
    await rowIn(page, ticker).click();
    await expect(row.getByTestId('thread-row-attention')).toHaveCount(0);
    await expect.poll(async () => (await rowStates(page, [ticker]))[ticker]).toMatchObject({ bold: false, status: null, attention: null });
    // The read reaches the backend after the row clears.
    await expect.poll(async () => {
      const read = await harness.rpc<{ attention?: { at: number } | null; lastReadAt?: number | null }>('GetThread', ticker);
      return read.attention != null && (read.lastReadAt ?? 0) >= read.attention.at;
    }, { message: 'the read covers the flag, which stays on the row' }).toBe(true);
  });
}

// The lines under a row each tuck under the one above by the same small
// overlap, whichever of them are present, so none covers another's text.
test('a flagged assistant card stacks its lines without covering one another', async ({ harness, page }) => {
  test.setTimeout(120_000);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'flagged-card', repo: {},
      threads: [{ title: 'Board agent', turns: [{ userText: 'hi', items: [{ kind: 'assistant_text', summary: 'ready' }] }] }],
    }],
  });
  const agent = seed.projects[0].threadIds[0];
  const { home } = await harness.rpc<{ home: string }>('HarnessMakeAssistant', agent);
  await setScenario(harness, home, threadToolsScenario({
    name: 'flagged-card',
    provider: 'claude',
    turns: [{
      steps: [
        { call: { tool: 'thread_loop', args: { label: 'board', note: 'look at the board', every_seconds: 3600 } } },
        { call: { tool: 'thread_attention', args: { kind: 'problem', reason: 'The board API keeps timing out' } } },
      ],
      text: 'Watching, and flagged.',
    }],
    afterTurns: 'silent',
  }));
  await harness.rpc('StartSession', agent);
  await harness.rpc('SendMessage', agent, 'watch the board', null);
  const flagged = await awaitToolAnswer(harness, { tool: 'thread_attention', cwd: home, timeoutMs: 60_000 });
  expect(flagged.isError, flagged.text).toBe(false);
  await awaitTurnCompleted(harness, agent);

  await harness.open(page);
  await awaitSidebarSettled(page);
  const card = cardShell(page, agent);
  const lines = ['thread-row-attention', 'thread-row-agent-state', 'thread-row-agent-projects'].map((id) => card.getByTestId(id));
  for (const line of lines) await expect(line).toBeVisible();
  const box = async (target: Locator) => (await target.boundingBox())!;
  let above = await box(cardRow(page, agent));
  // The first line tucks 6px under the row's own padding; each later one
  // 2px under the line above it.
  let tuck = 6;
  for (const line of lines) {
    const next = await box(line);
    expect(next.y, await line.getAttribute('data-testid') ?? '').toBeGreaterThanOrEqual(above.y + above.height - tuck - 0.5);
    above = next;
    tuck = 2;
  }
  await card.screenshot({ path: test.info().outputPath('flagged-card.png') });
});
