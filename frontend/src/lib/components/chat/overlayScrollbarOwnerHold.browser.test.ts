// The pane scrollbar's activity fade must attribute scroll events to the
// READER, not to the owner's own choreography.
//
// The 2026-08-29 report: the bar is meant to stay hidden unless the reader
// scrolls, but it faded in at the bottom right as a glide ended. Mechanism:
// every `preserveViewportBottom` transaction (the auto-collapse gate's
// releases, bulk collapse, restore snaps) runs under a `pauseAutoScroll`
// lease, and the timeline keyed `ownerDrivenPosition` on `stick.isSticky` —
// which demands `pauseDepth === 0`. So the transaction's own bottom-restore
// write and the browser's shrink-clamp arrived at the bar as "not owner
// driven" and revealed it, exactly when the reader had touched nothing. The
// gate cannot run during a glide (autoScrollInFlight stand-down), which is
// why the flash landed at glide-end, alongside the collapse work's frame
// cost. The fix keys the fade on `positionOwnerDriven` (!escapedFromLock):
// the position is the owner's until an explicit reader escape, and escape
// arms on the input event itself, before its scroll event dispatches.
//
// Phases are ordered inside one mount because the positive control escapes
// the controller, which would pollute the owner-attribution phases.
import { describe, expect, it } from 'vitest';
import '../../../app.css';
import { makeItem } from '../../../test/helpers/chat';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import {
  distanceToBottom,
  mountTimeline,
  seedTimelineItems,
  setupTimelineHarness,
  userScrollTo,
  waitForQuietBottom,
  type QuietBottomOptions,
} from '../../../test/helpers/timelineBrowserHarness';
import type { Item } from '../../types/models';

setupTimelineHarness();

const THREAD_ID = 'owner-hold-scrollbar';
const QUIET: QuietBottomOptions = { epsilonPx: 2, stableFrames: 12, frameBudget: 480 };

const PROSE = {
  question: (i: number) => `Question ${i}: does the owner's own write ever reveal the bar?`,
  replyLead: (i: number) =>
    `Reply ${i} is ordinary prose so rendered heights vary and the pane genuinely scrolls.`,
  replyList: `- a point\n- another point\n- a third with \`inline code\``,
};

// A settled activity run NEAR the bottom but not AT the tail: bulk collapse
// skips the tail run (tail-ness outranks the bulk state), and a collapsed
// clip must shrink in-viewport content so the browser's clamp really fires
// a scroll event during the transaction's pause window.
function transcript(): Item[] {
  const items = seedTimelineItems(THREAD_ID, PROSE);
  const runTurn = items.length;
  for (let i = 0; i < 5; i++) {
    items.push(
      makeItem({
        id: `run-tool-${i}`,
        threadId: THREAD_ID,
        turnIndex: runTurn,
        itemIndex: i,
        kind: 'tool_call',
        toolName: 'Bash',
        status: 'completed',
        summary: `command ${i} with enough summary text to give the row height`,
      }),
    );
  }
  items.push(
    makeItem({
      id: 'tail-question',
      threadId: THREAD_ID,
      turnIndex: runTurn + 1,
      itemIndex: 0,
      kind: 'user_text',
      role: 'user',
      status: 'completed',
      summary: 'A trailing question so the run above is settled history, not the tail.',
    }),
    makeItem({
      id: 'tail-answer',
      threadId: THREAD_ID,
      turnIndex: runTurn + 2,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'completed',
      summary:
        'A trailing answer with two sentences of prose. It keeps the newest revealed node after the activity run.',
    }),
  );
  return items;
}

function paneBar(host: HTMLElement): HTMLElement {
  const el = host.querySelector('[aria-label="Scroll message history"]');
  if (!(el instanceof HTMLElement)) throw new Error('pane overlay scrollbar not mounted');
  return el;
}

// The fade is the class pair, not computed opacity: the 150ms transition
// would make a computed read racy while the class flip is the decision.
function hidden(track: HTMLElement): boolean {
  return track.classList.contains('opacity-0');
}

describe('overlay scrollbar owner-hold attribution', () => {
  it('owner transactions never reveal the bar; a reader gesture does', async () => {
    const { pane, scrollEl, host } = await mountTimeline(THREAD_ID, transcript(), QUIET);
    const track = paneBar(host);

    // Precondition: the mount choreography (restore snap, warm-up settle)
    // is all owner writes — the bar must come up hidden.
    expect(hidden(track), 'bar must be hidden after mount settle').toBe(true);

    // Phase 1 — a bare pause lease. This is the deterministic shape of the
    // regression: at bottom, not escaped, lease held, a scroll event lands.
    // With the fade keyed on isSticky the lease alone flipped attribution.
    const controller = pane.scrollController;
    if (!controller) throw new Error('pane scroll controller not attached');
    const release = controller.pauseAutoScroll();
    try {
      scrollEl.dispatchEvent(new Event('scroll'));
      for (let frame = 0; frame < 6; frame++) {
        await raf();
        expect(hidden(track), `bar revealed under a pause lease (frame ${frame})`).toBe(true);
      }
    } finally {
      release();
    }

    // Phase 2 — the production transaction: bulk collapse shrinks the
    // settled run's clip inside the viewport, the browser clamps scrollTop,
    // and the transaction restores the bottom, all under its own lease.
    let scrollEvents = 0;
    const countScroll = (): void => {
      scrollEvents += 1;
    };
    scrollEl.addEventListener('scroll', countScroll);
    const revealedFrames: number[] = [];
    try {
      pane.activityRuns.setAllCollapsed(true);
      for (let frame = 0; frame < 30; frame++) {
        if (!hidden(track)) revealedFrames.push(frame);
        await raf();
      }
    } finally {
      scrollEl.removeEventListener('scroll', countScroll);
    }
    // Stimulus guard: a collapse that moved nothing would pass vacuously.
    expect(scrollEvents, 'the collapse must actually fire scroll events').toBeGreaterThan(0);
    expect(revealedFrames, 'bar revealed during an owner transaction').toEqual([]);
    await waitForQuietBottom(scrollEl, 'post-collapse settle', QUIET);
    expect(distanceToBottom(scrollEl)).toBeLessThanOrEqual(2);

    // Phase 3 — positive control, last because it escapes: a real reader
    // gesture (wheel + scroll) must still reveal the bar, or the fix would
    // be indistinguishable from never showing it.
    await userScrollTo(scrollEl, Math.max(0, scrollEl.scrollTop - 600));
    await waitFor(() => !hidden(track), 'reader gesture must reveal the bar');
  });
});
