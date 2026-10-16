// Session import driven through the REAL UI: the sidebar trigger, the lazy
// modal, its filters and selection, the run's progress channel, the threads
// that appear afterwards, and "Check for Provider Updates" on one of them.
//
// The provider homes are hand-written fixtures under the harness's isolated
// HOME (see session-import-fixtures.ts) — the same `credentialHomeOverride`
// seam the Go tests use, so nothing here can reach a real `~/.claude` /
// `~/.codex`. Everything downstream of that file write is production code:
// the scan, the import run, the progress events, the store writes and the
// projections the sidebar re-pulls.
//
// Two determinism notes:
//
//   - The happy-path run's progress strip is NOT asserted, because a clean
//     run closes the modal itself the moment it settles and a two-session
//     import settles in milliseconds — polling for the strip would be a
//     race. The strip, the per-row outcome stamps and the morphing CTA are
//     asserted instead on a run that FAILS a session, which is exactly the
//     run the surface deliberately stays open for.
//   - Backend progress is awaited on the wire (`session-import:progress`,
//     terminal frame) rather than inferred from the DOM.
//
// View-only/remote posture is out of scope here: the harness binds loopback,
// and what an off-host session may reach is pinned in
// harness-offhost-authz.spec.ts instead. The import RPCs are
// `threads:operate`, so a session holding that grant may run one.

import { test, expect } from './fixtures.js';
import {
  BRANCH_A_THINKING_PREFIX,
  BRANCH_B_THINKING_PREFIX,
  CODEX_ANSWER,
  CODEX_PROMPT,
  DELEGATED_ANSWER,
  DELEGATED_PROMPT,
  FORK_CHILD_ANSWER,
  FORK_CHILD_PROMPT,
  FORK_SHARED_ANSWER,
  SCHEDULED_ANSWER,
  SCHEDULED_PROMPT,
  growLinearClaudeSession,
  MCP_TEXT_HEAD,
  MCP_TEXT_TAIL,
  seedImportFixtures,
  type FixtureSession,
} from './session-import-fixtures.js';
import type { Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';

test.beforeEach(async ({ harness }) => {
  // Import assertions inspect tool/thinking rows. State that fixture policy
  // directly instead of inheriting the product's collapsed-run default.
  await harness.rpc('UpdateSettings', { activityRunDefault: 'expanded' });
});

/** One frame of `session-import:progress` (app_session_import_run.go). */
interface ImportProgressFrame {
  importId: string;
  completed: number;
  total: number;
  id?: string;
  status?: string;
  threadIds?: string[];
  error?: string;
  done?: boolean;
}

interface ImportedItemIdentity {
  id: string;
  kind: string;
  payloadId?: string;
}

interface ImportedThreadLineage {
  id: string;
  forkedFromThreadId?: string;
}

const openImportModal = async (page: Page) => {
  await page.getByTestId('sidebar-import-sessions-icon').click();
  await expect(page.getByTestId('session-import-body')).toBeVisible();
};

/** Await the run's terminal frame, which arrives after every per-row one. */
const waitForRunDone = (harness: HarnessApp) =>
  harness.waitForEvent<ImportProgressFrame>('session-import:progress', (evt) => evt.done === true);

/** A sidebar thread row carrying this title. */
const threadRow = (page: Page, title: string) =>
  page.getByTestId('thread-row').filter({ hasText: title });

/** Expand the sole thinking row and prove its full, thread-owned payload. */
async function expectThinkingPayload(
  page: Page,
  ownPrefix: string,
  foreignPrefix: string,
): Promise<void> {
  const toggle = page.getByTestId('thinking-toggle');
  const body = page.getByTestId('thinking-body');
  await expect(toggle).toHaveCount(1);
  if ((await toggle.getAttribute('aria-expanded')) !== 'true') await toggle.click();
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  await expect(body).toContainText(ownPrefix);
  await expect(body).not.toContainText(foreignPrefix);
}

/**
 * Select rows and press the primary. Returns the terminal frame plus each
 * row's own frame — the terminal one carries no per-row detail, so the thread
 * ids a session actually created are only ever on its own frame.
 *
 * Every wait is armed BEFORE the click so a run that finishes inside the RPC
 * round trip cannot be missed.
 */
async function importRows(
  page: Page,
  harness: HarnessApp,
  sessions: FixtureSession[],
): Promise<{ done: ImportProgressFrame; rows: Map<string, ImportProgressFrame> }> {
  for (const session of sessions) await page.getByTestId(session.rowTestId).click();
  await expect(page.getByTestId('session-import-confirm')).toHaveText(
    `Import (${sessions.length})`,
  );
  const rowFrames = sessions.map(async (session) => {
    const frame = await harness.waitForEvent<ImportProgressFrame>(
      'session-import:progress',
      (evt) => evt.id === session.rowId,
    );
    return [session.rowId, frame] as const;
  });
  const done = waitForRunDone(harness);
  await page.getByTestId('session-import-confirm').click();
  return { rows: new Map(await Promise.all(rowFrames)), done: await done };
}

test('lists sessions from both providers and narrows by provider filter and search', async ({
  harness,
  page,
}) => {
  const fx = await seedImportFixtures(harness);
  await harness.open(page);
  await openImportModal(page);

  await expect(page.getByTestId(fx.claudeLinear.rowTestId)).toContainText(fx.claudeLinear.title);
  await expect(page.getByTestId(fx.claudeBranched.rowTestId)).toContainText(
    fx.claudeBranched.title,
  );
  await expect(page.getByTestId(fx.codex.rowTestId)).toContainText(fx.codex.title);
  // The Task launch in the branched transcript has one subagent transcript
  // beside it, counted from `<session>/subagents/agent-*.jsonl` at list time.
  await expect(page.getByTestId(`session-import-subagents-${fx.claudeBranched.rowId}`)).toHaveText(
    '1 subagent',
    { useInnerText: true },
  );
  await expect(page.getByTestId('session-import-confirm')).toHaveText('Import all (3)');

  await page.getByRole('radio', { name: 'Codex' }).click();
  await expect(page.getByTestId(fx.codex.rowTestId)).toBeVisible();
  await expect(page.getByTestId(fx.claudeLinear.rowTestId)).toHaveCount(0);
  await expect(page.getByTestId('session-import-confirm')).toHaveText('Import all (1)');

  await page.getByRole('radio', { name: 'Claude' }).click();
  await expect(page.getByTestId(fx.codex.rowTestId)).toHaveCount(0);
  await expect(page.getByTestId('session-import-confirm')).toHaveText('Import all (2)');

  await page.getByRole('radio', { name: 'All' }).click();
  await page.getByTestId('session-import-search').fill('retry');
  await expect(page.getByTestId(fx.claudeLinear.rowTestId)).toBeVisible();
  await expect(page.getByTestId(fx.claudeBranched.rowTestId)).toHaveCount(0);
  await expect(page.getByTestId(fx.codex.rowTestId)).toHaveCount(0);
  await expect(page.getByTestId('session-import-confirm')).toHaveText('Import all (1)');

  // A query matching nothing is escapable — the toolbar stays, and the empty
  // state offers the reset.
  await page.getByTestId('session-import-search').fill('nothing matches this');
  await expect(page.getByTestId('session-import-no-matches')).toBeVisible();
  await page.getByTestId('session-import-clear-filters').click();
  await expect(page.getByTestId(fx.codex.rowTestId)).toBeVisible();
});

/**
 * The left edge of each column, per row. Rows are separate virtualized
 * elements, so nothing but a shared track template keeps them aligned; a
 * chip, a long title or a missing branch must not move any column.
 */
async function columnEdges(page: Page, rowTestIds: string[]): Promise<Map<string, number[]>> {
  const edges = new Map<string, number[]>();
  for (const testId of rowTestIds) {
    const row = page.getByTestId(testId);
    for (const col of ['title', 'project', 'branch', 'time']) {
      const cell = row.locator(`[data-col="${col}"]`);
      const box = (await cell.count()) > 0 && (await cell.isVisible()) ? await cell.boundingBox() : null;
      if (!box) continue;
      edges.set(col, [...(edges.get(col) ?? []), Math.round(box.x)]);
    }
  }
  return edges;
}

test('every import list column lines up across rows, wide and narrow', async ({ harness, page }) => {
  const fx = await seedImportFixtures(harness, {
    withDelegatedCodexSession: true,
    withLongTitleCodexRow: true,
  });
  const rows = [fx.claudeLinear, fx.claudeBranched, fx.codex, fx.codexDelegated!, fx.codexLongTitle!];
  const rowIds = rows.map((row) => row.rowTestId);

  for (const viewport of [
    { width: 1400, height: 900 },
    { width: 390, height: 844 },
  ]) {
    await page.setViewportSize(viewport);
    await harness.open(page);
    await openImportModal(page);
    // The branched Claude row carries the subagent chip; the long title
    // fills its track.
    const chip = page.getByTestId(`session-import-subagents-${fx.claudeBranched.rowId}`);
    await expect(chip).toHaveAttribute('aria-label', '1 subagent');
    for (const row of rows) await expect(page.getByTestId(row.rowTestId)).toBeVisible();

    const edges = await columnEdges(page, rowIds);
    for (const col of ['title', 'project', 'time']) {
      const xs = edges.get(col) ?? [];
      expect(xs, `${col} column at ${viewport.width}px`).toHaveLength(rows.length);
      expect(new Set(xs).size, `${col} column edges at ${viewport.width}px: ${xs}`).toBe(1);
    }
    const branches = edges.get('branch') ?? [];
    expect(new Set(branches).size, `branch column edges at ${viewport.width}px: ${branches}`).toBeLessThanOrEqual(1);

    // The title is the flexible track: it keeps real room at phone width,
    // and the row leaves no idle width while a title is cut short.
    const title = page.getByTestId(fx.codexLongTitle!.rowTestId).locator('[data-col="title"]');
    expect((await title.boundingBox())?.width ?? 0).toBeGreaterThan(80);
    const delegatedTitle = page
      .getByTestId(fx.codexDelegated!.rowTestId)
      .locator('[data-col="title"] > span')
      .first();
    const chipTitle = page
      .getByTestId(fx.claudeBranched.rowTestId)
      .locator('[data-col="title"] > span')
      .first();
    if (viewport.width >= 1000) {
      expect(
        await delegatedTitle.evaluate((el) => el.scrollWidth <= el.clientWidth),
        'the delegated title fits in full at 1400px',
      ).toBe(true);
      expect(
        await chipTitle.evaluate((el) => el.scrollWidth <= el.clientWidth),
        'a title beside its chip fits in full at 1400px',
      ).toBe(true);
      await expect(chip).toHaveText('1 subagent', { useInnerText: true });
      // The outcome track is one glyph wide: the age column ends at the row's
      // right padding, not in front of idle width.
      const row = await page.getByTestId(fx.codex.rowTestId).boundingBox();
      const time = await page.getByTestId(fx.codex.rowTestId).locator('[data-col="time"]').boundingBox();
      expect(row!.x + row!.width - (time!.x + time!.width)).toBeLessThan(48);
    } else {
      // Narrow: the chip collapses to its icon and count, so a short title
      // beside it still shows in full.
      await expect(chip).toHaveText('1', { useInnerText: true });
      expect(
        await chipTitle.evaluate((el) => el.scrollWidth <= el.clientWidth),
        'a short title beside its chip fits at 390px',
      ).toBe(true);
    }
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('session-import-body')).toHaveCount(0);
  }
});

test('the project dropdown paints above the modal and applies its filter', async ({
  harness,
  page,
}) => {
  await seedImportFixtures(harness);
  await harness.open(page);
  await openImportModal(page);

  // The menu is a Popover portaled to <body>, so it and the modal backdrop
  // stack in the same root context — this click is the paint-order guard:
  // with the popover layered below the backdrop (z-50 vs z-[60], the
  // original defect) the blurred backdrop is the hit target and the click
  // times out. Only a real-browser hit test can see that; unit tests can't.
  await page.getByTestId('session-import-project-trigger').click();
  const group = page.getByRole('menuitem', { name: /import-fixture-repo/ });
  await expect(group).toContainText('3');
  await group.click();

  await expect(page.getByTestId('session-import-project-trigger')).toContainText(
    'import-fixture-repo',
  );
  await expect(page.getByTestId('session-import-confirm')).toHaveText('Import all (3)');

  // And Escape while the reopened menu is up closes the MENU, not the modal.
  await page.getByTestId('session-import-project-trigger').click();
  await expect(page.locator('[data-popover]')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.locator('[data-popover]')).toHaveCount(0);
  await expect(page.getByTestId('session-import-body')).toBeVisible();
});

test('importing a selection creates the threads and renders their history', async ({
  harness,
  page,
}) => {
  const fx = await seedImportFixtures(harness);
  await harness.open(page);
  await openImportModal(page);

  const { done, rows } = await importRows(page, harness, [fx.claudeLinear, fx.codex]);
  expect(done.completed).toBe(2);
  expect(done.total).toBe(2);
  expect(rows.get(fx.claudeLinear.rowId)?.status).toBe('imported');
  expect(rows.get(fx.codex.rowId)?.status).toBe('imported');

  // A clean run closes its own surface and reports the threads actually
  // created rather than inferring success from the selected-row count.
  await expect(page.getByTestId('session-import-body')).toHaveCount(0);
  await expect(page.getByText('Imported 2 sessions (2 threads).')).toBeVisible();

  // The sidebar learns about imported threads (and their project) from the
  // done frame's projection refresh — no reload.
  await expect(threadRow(page, fx.claudeLinear.title)).toBeVisible();
  await expect(threadRow(page, fx.codex.title)).toBeVisible();

  await threadRow(page, fx.claudeLinear.title).click();
  await expect(page.getByText('Running the suite first.')).toBeVisible();
  await expect(page.getByText('Added the retry helper with a backoff test.')).toBeVisible();
  // The Bash call imported as an ordinary tool row — launch and result folded
  // into one card the way a live turn writes it, with the output in a payload
  // the row loads on demand.
  await expect(page.getByTestId('command-output-command')).toHaveText('Run the retry tests');
  await expect(page.getByTestId('command-output-command')).toHaveAttribute('title', 'go test ./internal/retry');
  await page.getByTestId('command-output-toggle').click();
  await expect(page.getByTestId('command-output-full-command')).toHaveText('go test ./internal/retry');
  await expect(page.getByText('ok  internal/retry 0.02s')).toBeVisible();

  await threadRow(page, fx.codex.title).click();
  await expect(page.getByText(CODEX_PROMPT).first()).toBeVisible();
  await expect(page.getByText(CODEX_ANSWER)).toBeVisible();
});

test('a turn the CLI started from a cron job imports with its prompt badged as scheduled', async ({
  harness,
  page,
}) => {
  const fx = await seedImportFixtures(harness, { withScheduledSession: true });
  const scheduled = fx.claudeScheduled!;
  await harness.open(page);
  await openImportModal(page);
  const { done } = await importRows(page, harness, [scheduled]);
  expect(done.completed).toBe(1);

  await threadRow(page, scheduled.title).click();
  const prompt = page.locator('[data-item-id][data-item-kind="user_text"]').filter({ hasText: SCHEDULED_PROMPT });
  await expect(prompt).toHaveCount(1);
  await expect(prompt.getByTestId('user-message-scheduled-origin')).toHaveText('Scheduled');
  const answer = page.locator('[data-item-id][data-item-kind="assistant_text"]').filter({
    has: page.getByText(SCHEDULED_ANSWER, { exact: true }),
  });
  await expect(answer).toBeVisible();
  const [promptBox, answerBox] = [await prompt.boundingBox(), await answer.boundingBox()];
  expect(promptBox!.y).toBeLessThan(answerBox!.y);
});

// Codex 0.162 shapes from a session started outside Agent Overflow: a thread
// Codex's app opened from another thread (`create_thread`) has no user
// message, only a `preview`, and its paginated rollout stores an MCP result
// over 64 KiB as a cut copy of the serialized result.
test('a Codex thread another Codex thread started lists by its prompt and opens with it', async ({
  harness,
  page,
}) => {
  const fx = await seedImportFixtures(harness, { withDelegatedCodexSession: true });
  const delegated = fx.codexDelegated!;
  await harness.open(page);
  await openImportModal(page);

  // The catalogue names it by the delegated prompt, Codex's `preview`.
  await expect(page.getByTestId(delegated.rowTestId)).toContainText(DELEGATED_PROMPT);

  const { rows } = await importRows(page, harness, [delegated]);
  expect(rows.get(delegated.rowId)?.status).toBe('imported');
  await expect(page.getByText('Imported 1 session (1 thread).')).toBeVisible();

  await threadRow(page, DELEGATED_PROMPT).click();
  const bubble = page.getByTestId('user-message-bubble');
  await expect(bubble).toHaveCount(1);
  await expect(bubble).toContainText(DELEGATED_PROMPT);
  await expect(page.getByTestId('user-message-codex-delegation-origin')).toHaveText(
    'From another Codex thread',
  );
  await expect(page.getByText(DELEGATED_ANSWER)).toBeVisible();

  // The MCP result reads as the server's text with Codex's cut marked, not
  // as clipped escaped JSON. Its call-less twins add no second card.
  const card = page.getByTestId('tool-call-card');
  await expect(card).toHaveCount(1);
  await card.getByTestId('tool-call-card-toggle').click();
  const output = card.getByTestId('tool-call-card-output');
  await expect(output).toContainText(MCP_TEXT_HEAD);
  const showFull = card.getByTestId('tool-call-card-show-full');
  if (await showFull.isVisible()) await showFull.click();
  await expect(output).toContainText(
    'Codex truncated this MCP result when it saved the session:',
  );
  await expect(output).toContainText(MCP_TEXT_TAIL);
  await expect(output).not.toContainText('{"content"');
  await expect(output).not.toContainText('model-visible copy');
});

test('a multi-leaf Claude transcript imports one coherent active thread', async ({
  harness,
  page,
}) => {
  const fx = await seedImportFixtures(harness);
  await harness.open(page);
  await openImportModal(page);

  const { rows } = await importRows(page, harness, [fx.claudeBranched]);
  const threadIDs = rows.get(fx.claudeBranched.rowId)?.threadIds ?? [];
  expect(threadIDs).toHaveLength(1);
  await expect(page.getByText('Imported 1 session (1 thread).')).toBeVisible();

  const items = await harness.rpc<ImportedItemIdentity[] | null>('ListItems', threadIDs[0], true);
  const thinking = (items ?? []).filter((item) => item.kind === 'thinking');
  expect(thinking).toHaveLength(1);
  expect(thinking[0]?.payloadId).toBe('thinking:think:2:0');

  // A provider session has one stable AO identity regardless of how many
  // abandoned leaves its transcript retains. Branch prompts never become
  // sibling sidebar rows or leak into the imported thread's title.
  await expect(page.getByTestId('thread-row')).toHaveCount(1);
  await expect(threadRow(page, fx.claudeBranched.title)).toBeVisible();
  await expect(page.getByTestId('thread-row')).not.toContainText('Document the parser');
  await expect(page.getByTestId('thread-row')).not.toContainText('Benchmark the parser');

  // The active chain is coherent: shared ancestry and the current continuation
  // render, while the abandoned sibling never enters the timeline.
  await threadRow(page, fx.claudeBranched.title).click();
  await expect(page.getByText('Parsed it.')).toBeVisible();
  await expect(page.getByText('Benchmarked the parser at 120ns/op.')).toBeVisible();
  await expect(page.getByText('Documented the parser.')).toHaveCount(0);
  await expectThinkingPayload(page, BRANCH_B_THINKING_PREFIX, BRANCH_A_THINKING_PREFIX);
});

for (const childFirst of [false, true]) {
test(`an explicit Claude fork exposes navigable lineage (${childFirst ? 'separate child-first runs' : 'concurrent import'})`, async ({
  harness,
  page,
}) => {
  const fx = await seedImportFixtures(harness, { withForkedSession: true });
  const fork = fx.claudeFork;
  if (!fork) throw new Error('fixture did not write the explicit fork session');
  await harness.open(page);
  await openImportModal(page);

  await expect(page.getByTestId(fx.claudeLinear.rowTestId)).toBeVisible();
  await expect(page.getByTestId(fork.rowTestId)).toBeVisible();
  let rows: Map<string, ImportProgressFrame>;
  if (childFirst) {
    const childRun = await importRows(page, harness, [fork]);
    await expect(threadRow(page, fork.title)).toBeVisible();
    await expect(threadRow(page, fork.title).getByTestId('thread-row-fork-lineage')).toHaveCount(0);
    await openImportModal(page);
    const parentRun = await importRows(page, harness, [fx.claudeLinear]);
    rows = new Map([...childRun.rows, ...parentRun.rows]);
  } else {
    ({ rows } = await importRows(page, harness, [fork, fx.claudeLinear]));
  }
  const parentIDs = rows.get(fx.claudeLinear.rowId)?.threadIds ?? [];
  const childIDs = rows.get(fork.rowId)?.threadIds ?? [];
  expect(parentIDs).toHaveLength(1);
  expect(childIDs).toHaveLength(1);

  // The backend relation is exact even though the concurrent import run may
  // finish parent or child first.
  const stored = await harness.rpc<ImportedThreadLineage[]>('ListThreads');
  const child = stored.find((thread) => thread.id === childIDs[0]);
  expect(child?.forkedFromThreadId).toBe(parentIDs[0]);

  const parentRow = threadRow(page, fx.claudeLinear.title);
  const childRow = threadRow(page, fork.title);
  await expect(parentRow).toBeVisible();
  await expect(childRow).toBeVisible();
  const lineage = childRow.getByTestId('thread-row-fork-lineage');
  await expect(lineage).toBeEnabled();
  await expect(lineage).toHaveAttribute(
    'aria-label',
    `Open fork parent ${fx.claudeLinear.title}`,
  );
  const history = page.getByRole('log', { name: 'Message History' });

  await parentRow.click();
  await expect(history.getByText(FORK_SHARED_ANSWER)).toBeVisible();
  await expect(history.getByText(FORK_CHILD_PROMPT)).toHaveCount(0);
  await expect(history.getByText(FORK_CHILD_ANSWER)).toHaveCount(0);

  await childRow.click();
  await expect(history.getByText(FORK_SHARED_ANSWER)).toBeVisible();
  await expect(history.getByText(FORK_CHILD_PROMPT)).toBeVisible();
  await expect(history.getByText(FORK_CHILD_ANSWER)).toBeVisible();

  // The sidebar affordance must use the imported relation, not merely draw
  // an icon: clicking it opens the parent and removes the child's divergent
  // continuation from the visible timeline.
  await lineage.click();
  await expect(history.getByText(FORK_SHARED_ANSWER)).toBeVisible();
  await expect(history.getByText(FORK_CHILD_PROMPT)).toHaveCount(0);
  await expect(history.getByText(FORK_CHILD_ANSWER)).toHaveCount(0);
});

}

test('sessions already imported are gone from the next scan', async ({ harness, page }) => {
  const fx = await seedImportFixtures(harness);
  await harness.open(page);
  await openImportModal(page);

  const done = waitForRunDone(harness);
  await expect(page.getByTestId('session-import-confirm')).toHaveText('Import all (3)');
  await page.getByTestId('session-import-confirm').click();
  expect((await done).completed).toBe(3);
  await expect(page.getByTestId('session-import-body')).toHaveCount(0);
  await expect(page.getByText('Imported 3 sessions (3 threads).')).toBeVisible();

  // Reopening rescans (a run invalidates both caches), and the dedup set —
  // the source session ids the import recorded — subtracts every row.
  await openImportModal(page);
  await expect(page.getByTestId('session-import-empty')).toBeVisible();
  await expect(page.getByTestId(fx.claudeLinear.rowTestId)).toHaveCount(0);
  await expect(page.getByTestId(fx.codex.rowTestId)).toHaveCount(0);
});

test('a session that fails to import keeps the surface open with its stamps', async ({
  harness,
  page,
}) => {
  const fx = await seedImportFixtures(harness, { withFailingSession: true });
  const broken = fx.claudeBroken;
  if (!broken) throw new Error('fixture did not write the failing session');

  await harness.open(page);
  await openImportModal(page);

  const { done, rows } = await importRows(page, harness, [broken]);
  expect(done.completed).toBe(1);
  expect(rows.get(broken.rowId)?.status).toBe('failed');

  // Nothing landed, so there is something for the user to look at: the strip
  // stays, the row carries its own outcome, and the primary morphs into a
  // retry over exactly the failures.
  await expect(page.getByTestId('session-import-progress-headline')).toHaveText('Imported 0 of 1');
  await expect(page.getByTestId('session-import-progress-detail')).toHaveText('1 failed');
  await expect(page.getByTestId(`session-import-outcome-${broken.rowId}`)).toBeVisible();
  await expect(page.getByTestId('session-import-confirm')).toHaveText('Retry failed (1)');

  // The other rows are untouched and still importable — one broken transcript
  // never takes the rest of the catalogue with it.
  await page.getByTestId('session-import-cancel').click();
  await openImportModal(page);
  await expect(page.getByTestId(fx.claudeLinear.rowTestId)).toBeVisible();
  await expect(page.getByTestId(broken.rowTestId)).toBeVisible();
});

test('Check for Provider Updates appends what the session file grew', async ({
  harness,
  page,
}) => {
  const fx = await seedImportFixtures(harness);
  await harness.open(page);
  await openImportModal(page);
  await importRows(page, harness, [fx.claudeLinear]);

  await threadRow(page, fx.claudeLinear.title).click();
  await expect(page.getByText('Added the retry helper with a backoff test.')).toBeVisible();

  // The provider kept going in its own CLI after the import.
  const grown = await growLinearClaudeSession(fx);
  await expect(page.getByText(grown.answer)).toHaveCount(0);

  await threadRow(page, fx.claudeLinear.title).click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Check for Provider Updates' }).click();

  // The check builds the rows an apply WOULD write, so the dialog's counts
  // are exact rather than estimated.
  const dialog = page.getByRole('dialog', { name: 'Import Provider Updates' });
  await expect(dialog).toContainText('2 new messages and 1 turn can be added from the session file.');
  await dialog.getByRole('button', { name: 'Import', exact: true }).click();

  await expect(page.getByText('Imported 2 new items across 1 turn.')).toBeVisible();
  await expect(page.getByText(grown.prompt)).toBeVisible();
  await expect(page.getByText(grown.answer)).toBeVisible();
});
