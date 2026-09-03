// The sidebar badges that survived wave 6d2, proven on the clients the
// badges exist FOR: the ones with no pane on the thread.
//
// WHY THIS LEVEL. Narrowing `provider:item_event` per watch set moved two
// sidebar facts off the transcript stream and onto wildcard carriers: the
// Failed pill now rides `thread:error_notice`, and the Plan Ready pill
// rides the durable `hasActionableProposedPlan` column restated by a
// `thread:updated` full row. Both re-homings are argued in
// internal/transport/event_channels.go and pinned as units
// (internal/triage/router_test.go, frontend .../events.test.ts,
// ThreadRow.test.ts). What no unit can show is that the PRODUCING path
// still reaches them: that a provider turn ending in a persisted error row,
// and a turn proposing a plan, each light the pill on clients whose panes
// name some other thread, or no thread at all.
//
// So nothing here emits on a carrier channel. Both badges are driven from
// the mock provider's wire — a `system/model_refusal_no_fallback` line for
// the error (the one refusal family member that ends the turn with nothing,
// so triage persists kind `error` rather than `api_error`, which
// deliberately does not notify), and an `ExitPlanMode` tool call for the
// plan.
//
// THE TOPOLOGY, and why no client here is looking at the thread under test.
// A FOCUSED pane clears its own thread's Failed pill on sight (ChatView's
// projectThreadViewed effect: the user is reading the failure, so the
// sidebar stops advertising it). A spec that drove the turn from a pane on
// that thread would therefore be asserting a badge on the one client
// designed to drop it. Both clients here are sidebar-only for the thread
// the turns run on — one watching a DIFFERENT thread, one watching NOTHING,
// which are also the two shapes a watch set takes: a named entity, and the
// empty set a client with no panes states at boot.
//
// THE DROP PROOF is ordering, not absence. Both carriers are emitted
// immediately AFTER the item upsert they accompany, on the same connection:
// `persistItemWithEmit` calls `emitItemUpsert` then `emitErrorNotice`, and
// `handleProposedPlan` emits the item upsert then `emitThreadRow`. Frames
// are delivered in order, so a client that received the carrier and never
// received the item event for that thread did not receive it late — it was
// withheld. The harness's own connection sends no watch frame and stays
// wildcard, so awaiting the item event there proves the frames the pages
// never saw were emitted at all.
import type { BrowserContext, Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { test, expect, type SeedResult } from './fixtures.js';
import {
  RESULT_LINE,
  type ScenarioStep,
  emit,
  listItems,
  startMock,
  textLines,
  toolUseLine,
} from './agent-visibility-helpers.js';
import { readWire, receivedOn, recordWire, watchedNow } from './transport-watch-helpers.js';

/** The thread the provider turns run on. No client opens it. */
const SUBJECT_TITLE = 'Unattended thread';
/** The thread the first client has open, so its watch set names something. */
const NEIGHBOUR_TITLE = 'Neighbour thread';

const REFUSAL_SUMMARY = 'The mock provider refused this request and had nowhere to fall back to.';
const SUBJECT_RECOVERY = 'The unattended thread recovered on its next turn.';
const NEIGHBOUR_ANSWER = 'The neighbour thread streamed its own answer.';
const PLAN_MARKDOWN = '## Mock plan\n\n- Read the README\n- Change nothing\n';

/**
 * `system/model_refusal_no_fallback`: the request was refused and no
 * fallback route matched, so the turn produces nothing. Its meta carries
 * neither `fatal` nor an SDK error enum, which is what makes triage persist
 * it as kind `error` — the exact kind `emitErrorNotice` announces — while
 * leaving the session alive to close the turn with a normal result.
 */
const REFUSAL_LINE = JSON.stringify({
  type: 'system',
  subtype: 'model_refusal_no_fallback',
  original_model: 'claude-mock-1',
  api_refusal_category: 'policy',
  content: REFUSAL_SUMMARY,
});

/**
 * A multi-turn Claude scenario. `agent-visibility-helpers.claudeScenario`
 * scripts exactly one turn; the error case needs a second one to take the
 * thread back to a clean state on the record.
 */
function claudeTurns(name: string, turns: ScenarioStep[][]): unknown {
  return {
    version: 1,
    name,
    provider: 'claude',
    turns: turns.map((steps, index) => ({ label: `${name}-${index + 1}`, steps })),
    // A stray extra send must not replay the last turn's frames on top of
    // the state an assertion is about to read.
    afterTurns: 'silent',
  };
}

interface TwoThreadFixture {
  subjectThread: string;
  subjectPath: string;
  neighbourThread: string;
  neighbourPath: string;
}

/**
 * Two projects, one thread each, both with a completed turn so both are
 * sidebar-visible (drafts are hidden). Two PROJECTS rather than two threads
 * in one, because a scenario rule is scoped by workspace: one repo would
 * hand both threads the same script.
 */
async function seedTwoProjects(harness: HarnessApp): Promise<TwoThreadFixture> {
  const seedProject = (name: string, title: string) => ({
    name,
    repo: { commits: [{ message: 'init', files: { 'README.md': `# ${name}\n` } }] },
    threads: [
      {
        title,
        turns: [
          { userText: 'set the stage', items: [{ kind: 'assistant_text', summary: 'Ready.' }] },
        ],
      },
    ],
  });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      seedProject('watch-badge-subject', SUBJECT_TITLE),
      seedProject('watch-badge-neighbour', NEIGHBOUR_TITLE),
    ],
  });
  const [subject, neighbour] = seed.projects;
  expect(subject?.threadIds, 'the subject project must seed one visible thread').toHaveLength(1);
  expect(neighbour?.threadIds, 'the neighbour project must seed one visible thread').toHaveLength(1);
  return {
    subjectThread: subject!.threadIds[0]!,
    subjectPath: subject!.path,
    neighbourThread: neighbour!.threadIds[0]!,
    neighbourPath: neighbour!.path,
  };
}

/** The sidebar row for one thread. Carries the resolved pill status. */
function threadRow(page: Page, threadId: string) {
  return page.locator(`[data-sidebar-thread-id="${threadId}"]`);
}

/** Assert one sidebar row's pill, by status token and by the label a screen reader reads. */
async function expectPill(page: Page, threadId: string, status: string, label: string) {
  const row = threadRow(page, threadId);
  await expect(row).toHaveAttribute('data-effective-status', status);
  await expect(row.getByTestId('thread-row-status-dot')).toHaveAttribute('aria-label', label);
}

/**
 * Open a recorded client and confirm the watch set the backend holds for
 * it. `title` names the thread to open, or null for a client that opens
 * none — which STATES the empty set rather than staying wildcard, and a
 * wildcard client would receive everything and prove nothing.
 */
async function openWatching(
  harness: HarnessApp,
  page: Page,
  title: string | null,
  watched: string[],
): Promise<void> {
  await recordWire(page);
  await harness.open(page);
  if (title !== null) await page.getByText(title).click();
  await expect.poll(async () => watchedNow(await readWire(page))).toEqual(watched);
}

/** Both sidebar-only clients. */
interface Clients {
  /** Watches the neighbour thread — a non-empty watch set naming another entity. */
  neighbour: Page;
  /** Watches nothing — the empty set a client with no panes states at boot. */
  bare: Page;
}

async function openSidebarOnlyClients(
  harness: HarnessApp,
  neighbourPage: Page,
  context: BrowserContext,
  fixture: TwoThreadFixture,
): Promise<Clients> {
  // BARE FIRST, and the order is load-bearing. Pane layout persists in the
  // `client:<id>` ui_state bucket, and every page this backend hands out
  // carries the SAME client id, so a page that boots after another opened a
  // pane restores that pane and watches its thread. `HarnessReset` clears
  // ui_state, so the first page of each test is the one that can boot with
  // no panes at all.
  const bare = await context.newPage();
  await openWatching(harness, bare, null, []);
  await openWatching(harness, neighbourPage, NEIGHBOUR_TITLE, [fixture.neighbourThread]);
  for (const client of [neighbourPage, bare]) {
    // Both clients LIST the subject thread, so a badge has somewhere to
    // paint, and neither has a pane on it.
    await expect(threadRow(client, fixture.subjectThread)).toHaveCount(1);
  }
  return { neighbour: neighbourPage, bare };
}

test('an error row on an unwatched thread paints Failed on every sidebar, and its item events reach none of them', async ({
  harness,
  page,
  browser,
}) => {
  const fixture = await seedTwoProjects(harness);
  await harness.rpc('HarnessSetScenario', {
    cwd: fixture.subjectPath,
    scenario: claudeTurns('badge-error-subject', [
      [emit([REFUSAL_LINE, RESULT_LINE])],
      [emit([...textLines('msg-subject-recovery', SUBJECT_RECOVERY), RESULT_LINE])],
    ]),
  });
  await harness.rpc('HarnessSetScenario', {
    cwd: fixture.neighbourPath,
    scenario: claudeTurns('badge-error-neighbour', [
      [emit([...textLines('msg-neighbour', NEIGHBOUR_ANSWER), RESULT_LINE])],
    ]),
  });

  const bareContext = await browser.newContext();
  try {
    const clients = await openSidebarOnlyClients(harness, page, bareContext, fixture);

    // ---- the real path: a turn that ends with a persisted error row ----
    await startMock(harness, fixture.subjectThread);
    await harness.rpc('SendMessage', fixture.subjectThread, 'do the thing', null);
    // The harness connection never sent a watch frame, so it is wildcard:
    // seeing the transcript stream here proves the frames exist to withhold.
    await harness.waitForEvent<{ threadId: string }>(
      'provider:item_event',
      (event) => event.threadId === fixture.subjectThread,
    );
    await harness.waitForEvent<{ threadId: string }>(
      'provider:turn_completed',
      (event) => event.threadId === fixture.subjectThread,
    );

    // The persist actually happened — the badge reports a row, not a
    // frontend guess. `error`, not `api_error`: only the former notifies.
    const items = await listItems(harness, fixture.subjectThread);
    expect(
      items.filter((item) => item.kind === 'error').map((item) => item.summary),
      'the refusal must persist as an item of kind `error`',
    ).toEqual([REFUSAL_SUMMARY]);

    await expectPill(clients.neighbour, fixture.subjectThread, 'error', 'Failed');
    await expectPill(clients.bare, fixture.subjectThread, 'error', 'Failed');

    // ---- the badge clears on both, from the user's next message ----
    // Two wildcard carriers reach a sidebar-only client from that send —
    // the `thread:updated` activity patch and `provider:turn_started` — and
    // this level cannot separate them, because every state in which the
    // pill is READABLE is a state with no turn running, so a real send
    // always produces both. Which one clears it in isolation is pinned by
    // frontend/src/lib/stores/events.test.ts ("clears an error badge from a
    // thread:updated activity patch"); what this asserts is the outcome.
    await harness.rpc('SendMessage', fixture.subjectThread, 'try again', null);
    await harness.waitForEvent<{ threadId: string }>(
      'provider:turn_completed',
      (event) => event.threadId === fixture.subjectThread,
    );
    for (const client of [clients.neighbour, clients.bare]) {
      await expect(threadRow(client, fixture.subjectThread)).toHaveAttribute(
        'data-effective-status',
        'idle',
      );
    }

    // ---- the second ordering barrier: a turn the first client watches ----
    await startMock(harness, fixture.neighbourThread);
    await harness.rpc('SendMessage', fixture.neighbourThread, 'answer me', null);
    await harness.waitForEvent<{ threadId: string }>(
      'provider:turn_completed',
      (event) => event.threadId === fixture.neighbourThread,
    );
    await expect(clients.neighbour.getByText(NEIGHBOUR_ANSWER)).toBeVisible();

    // ---- the frame count ----
    const neighbourWire = await readWire(clients.neighbour);
    const bareWire = await readWire(clients.bare);
    const neighbourItemThreads = receivedOn(neighbourWire, 'provider:item_event');
    expect(
      neighbourItemThreads.filter((id) => id === fixture.subjectThread),
      'provider:item_event is entity-filtered: not one frame for the unwatched thread',
    ).toEqual([]);
    expect(
      neighbourItemThreads,
      'the capture must have seen the channel work, or the emptiness above proves nothing',
    ).toContain(fixture.neighbourThread);
    // The empty watch set narrows the channel wholesale, not just the
    // entities it omits: this client named no thread and gets no transcript.
    expect(
      receivedOn(bareWire, 'provider:item_event'),
      'a client with no pane watches the empty set and receives no transcript stream',
    ).toEqual([]);
    for (const wire of [neighbourWire, bareWire]) {
      expect(
        receivedOn(wire, 'thread:error_notice'),
        'the Failed carrier is wildcard and must arrive for a thread with no pane',
      ).toContain(fixture.subjectThread);
      expect(
        receivedOn(wire, 'thread:updated'),
        'the activity carrier is wildcard and must arrive for a thread with no pane',
      ).toContain(fixture.subjectThread);
    }
  } finally {
    await bareContext.close();
  }
});

test('a proposed plan on an unwatched thread paints Plan Ready on every sidebar', async ({
  harness,
  page,
  browser,
}) => {
  const fixture = await seedTwoProjects(harness);
  await harness.rpc('HarnessSetScenario', {
    cwd: fixture.subjectPath,
    scenario: claudeTurns('badge-plan-subject', [
      [
        emit([
          toolUseLine('msg-plan', 'toolu-plan', 'ExitPlanMode', { plan: PLAN_MARKDOWN }),
          RESULT_LINE,
        ]),
      ],
    ]),
  });

  const bareContext = await browser.newContext();
  try {
    const clients = await openSidebarOnlyClients(harness, page, bareContext, fixture);

    await startMock(harness, fixture.subjectThread);
    await harness.rpc('SendMessage', fixture.subjectThread, 'propose something', null);
    await harness.waitForEvent<{ threadId: string }>(
      'provider:item_event',
      (event) => event.threadId === fixture.subjectThread,
    );
    await harness.waitForEvent<{ threadId: string }>(
      'provider:turn_completed',
      (event) => event.threadId === fixture.subjectThread,
    );

    // The durable column the pill reads is what the plan write produced.
    const rows = await harness.rpc<Array<{ id: string; hasActionableProposedPlan: boolean }>>(
      'HarnessListThreadRows',
    );
    expect(
      rows.find((row) => row.id === fixture.subjectThread)?.hasActionableProposedPlan,
      'the ExitPlanMode call must leave an actionable proposed plan on the row',
    ).toBe(true);

    await expectPill(clients.neighbour, fixture.subjectThread, 'plan-ready', 'Plan Ready');
    await expectPill(clients.bare, fixture.subjectThread, 'plan-ready', 'Plan Ready');

    // Ordering: `handleProposedPlan` emits the item upsert and only then
    // the full row. Both clients hold the row and not the upsert, on one
    // ordered connection each — the item event was withheld, not delayed.
    for (const wire of [await readWire(clients.neighbour), await readWire(clients.bare)]) {
      expect(
        receivedOn(wire, 'thread:updated'),
        'the Plan Ready carrier is wildcard and must arrive for a thread with no pane',
      ).toContain(fixture.subjectThread);
      expect(
        receivedOn(wire, 'provider:item_event').filter((id) => id === fixture.subjectThread),
        'the plan item upsert precedes that row and must still have been withheld',
      ).toEqual([]);
    }
  } finally {
    await bareContext.close();
  }
});
