// The 2026-08-29 bookmarked-recovery scenario, replayed end to end over the
// REAL MessageTimeline (real windowing, real warm-up gate, real scroll
// controller, real fonts/layout).
//
// The production incident chained four failures out of one transcript shape —
// a Codex detached launch whose mailbox delivered several durable completion
// rows:
//
//   1. the projection minted one card per delivery, all under one Svelte key,
//      and the keyed `{#each}` threw `each_key_duplicate`, aborting update
//      batches until the pane read as frozen;
//   2. reopening the pane misclassified the first populated mount as a
//      placeholder materialization, so restore consent was never armed and the
//      transcript sat blank at scrollTop=0 under a sticky-bottom claim;
//   3. the content-geometry sample published before the controller attached
//      was dropped and deduped away, pinning the wrong bottom;
//   4. the visible symptom of 1-3 was a multi-second spring chase.
//
// Each repair carries its own red-capable unit proof (subagentGrouping.test.ts,
// timelineRestoreSwitchEdge.svelte.test.ts, contentGeometrySubscription.test.ts).
// This suite is the composite outcome claim: the whole scenario mounts,
// settles at the true bottom inside the harness's bounded frame budget (a
// spring chase blows it), renders ONE card for the multi-delivery launch,
// keeps every run-child key unique, follows a live fourth delivery, and
// crosses the entire scenario without a single window error — the freeze was
// nothing but an uncaught throw inside an update batch.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { makeItem } from '../../../test/helpers/chat';
import { waitFor } from '../../../test/helpers/browserFrames';
import {
  distanceToBottom,
  mountTimeline,
  seedTimelineItems,
  setupTimelineHarness,
  waitForQuietBottom,
  type QuietBottomOptions,
} from '../../../test/helpers/timelineBrowserHarness';
import type { Item } from '../../types/models';

setupTimelineHarness();

const THREAD_ID = 'bookmarked-recovery';
const QUIET_BOTTOM: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };

const PROSE = {
  question: (i: number) => `Question ${i}: enough prose to give the row a real height?`,
  replyLead: (i: number) =>
    `Reply ${i} carries ordinary prose so rendered heights vary and the markdown pipeline is genuinely exercised.`,
  replyList: `- one point\n- another point\n- a third with \`inline code\``,
};

// The incident's transcript shape, verbatim from the production snapshot's
// structure: a background collab_agent spawn (detached — no wait carrier
// claims it) whose completion siblings are content-hashed mailbox deliveries.
function spawn(id: string, turnIndex: number, itemIndex: number): Item {
  return makeItem({
    id,
    threadId: THREAD_ID,
    turnIndex,
    itemIndex,
    kind: 'tool_call',
    toolName: 'collab_agent',
    isBackground: true,
    status: 'completed',
    summary: 'spawn',
    meta: JSON.stringify({ toolName: 'collab_agent', input: { tool: 'spawn_agent' } }),
  });
}

function delivery(launchId: string, hash: string, turnIndex: number, itemIndex: number): Item {
  return makeItem({
    id: `complete:${launchId}:delivery:${hash}`,
    threadId: THREAD_ID,
    turnIndex,
    itemIndex,
    kind: 'tool_completion',
    toolName: 'collab_agent',
    isBackground: true,
    status: 'completed',
    completionOf: launchId,
    summary: `delivery ${hash}`,
  });
}

function incidentTranscript(): Item[] {
  const items = seedTimelineItems(THREAD_ID, PROSE);
  const turn = items.length;
  items.push(
    spawn('spawn-1', turn, 0),
    delivery('spawn-1', 'aaa', turn, 1),
    delivery('spawn-1', 'bbb', turn, 2),
    delivery('spawn-1', 'ccc', turn, 3),
  );
  return items;
}

function runChildKeys(host: HTMLElement): string[] {
  return [...host.querySelectorAll('[data-run-child]')].map(
    (el) => el.getAttribute('data-run-child') ?? '',
  );
}

describe('bookmarked recovery scenario', () => {
  const windowErrors: string[] = [];
  const onError = (event: ErrorEvent): void => {
    windowErrors.push(String(event.error ?? event.message));
  };
  const onRejection = (event: PromiseRejectionEvent): void => {
    windowErrors.push(String(event.reason));
  };

  beforeEach(() => {
    windowErrors.length = 0;
    window.addEventListener('error', onError);
    window.addEventListener('unhandledrejection', onRejection);
  });

  afterEach(() => {
    window.removeEventListener('error', onError);
    window.removeEventListener('unhandledrejection', onRejection);
  });

  it('mounts, settles at the true bottom, renders one card, and follows a live delivery', async () => {
    // mountTimeline IS the incident's reopen: the first mount of an already
    // populated pane, through the real warm-up gate and restore choreography.
    // Its quiet-bottom wait carries the anti-spring claim — a chase that
    // outlives the frame budget fails the mount, and a blank pane fails the
    // visible-row wait inside it.
    const { pane, scrollEl, host } = await mountTimeline(
      THREAD_ID,
      incidentTranscript(),
      QUIET_BOTTOM,
      { provider: 'codex' },
    );

    // The true bottom, not a claim over scrollTop=0.
    expect(scrollEl.scrollTop).toBeGreaterThan(0);
    expect(distanceToBottom(scrollEl)).toBeLessThanOrEqual(2);

    // One launch, three durable deliveries → ONE card; the later deliveries
    // stay chronological leaves. Every keyed row in the run is unique — the
    // duplicate that froze production cannot re-enter the DOM silently.
    expect(host.querySelectorAll('[data-testid="subagent-group"]')).toHaveLength(1);
    const keys = runChildKeys(host);
    expect(keys.length).toBeGreaterThanOrEqual(4);
    expect(new Set(keys).size).toBe(keys.length);

    // A fourth delivery lands on the REAL wire path mid-session. Before the
    // one-card repair this minted a second card under the same key inside a
    // live update batch — the exact throw the error log tied to the frozen
    // pane and the truncated messages.
    pane.applyProviderItemUpserts([delivery('spawn-1', 'ddd', 45, 4)]);
    await waitFor(
      () => host.querySelectorAll('[data-run-child]').length === keys.length + 1,
      'fourth delivery to render as its own leaf',
    );
    await waitForQuietBottom(scrollEl, 'post-delivery settle', QUIET_BOTTOM);

    expect(host.querySelectorAll('[data-testid="subagent-group"]')).toHaveLength(1);
    const grownKeys = runChildKeys(host);
    expect(new Set(grownKeys).size).toBe(grownKeys.length);
    expect(distanceToBottom(scrollEl)).toBeLessThanOrEqual(2);

    expect(windowErrors, 'the scenario must cross without a single window error').toEqual([]);
  });
});
