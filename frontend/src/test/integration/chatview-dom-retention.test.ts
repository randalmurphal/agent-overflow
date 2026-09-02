// DOM-retention regression suite: a closed pane / companion must leave
// NO strong path from long-lived module state (stores, svelte-internal
// reaction graphs, event registries) to its DOM.
//
// Motivation (2026-07-20 live-app heap analysis): a production renderer
// held two full detached review-pane trees and a closed pane section
// (~23k nodes). The GC roots were browser-side pins (Blink session-
// history form state on a <select>; a C++ WebNode handle on an old
// composer textarea) — bounded slots — but the AMPLIFICATION was
// app-side: one pinned leaf element retained a 10k-node tree through
// listener-closure scope chains into long-lived module state. The
// concrete bug found by this probe: proposedPlans'
// retainProposedPlanEventListener registered a singleton onItemUpsert
// callback DEFINED INSIDE the retain call, so the callback's closure
// context captured the first caller's `threadIdScope` (a Composer
// closure over its pane) for as long as any pane held the listener —
// pinning the first pane's whole subtree after it closed.
//
// Technique: mount for real, WeakRef the DOM, close, force GC, assert
// collected. Probe subtleties:
//  - stack locals count as GC roots: element/pane references must stay
//    confined to inner function scopes.
//  - svelte keeps the last delegated event in a module slot
//    (`last_propagated_event`); svelte through 5.56.8 never cleared it,
//    so its `target` would retain a just-closed tree until the next user
//    event. Upstream 5.57.0 clears the slot a macrotask after each
//    dispatch (sveltejs/svelte#18569; focused regression:
//    svelte-patch-event-slot.test.ts), so this suite needs no flush —
//    the settle waits in collectHard let the clear fire.
//
// Needs --expose-gc; the suite skips (loudly) without it. The
// `make test` gate runs without it — run explicitly via:
//   NODE_OPTIONS=--expose-gc pnpm exec vitest run --project unit \
//     src/test/integration/chatview-dom-retention.test.ts
import { describe, expect, it, beforeAll, beforeEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import ChatView from '../../lib/components/chat/ChatView.svelte';
import ReviewPane from '../../lib/components/review/ReviewPane.svelte';
import PaneHost from '../../lib/components/panes/PaneHost.svelte';
import type { PanelContext } from '../../lib/stores/panelContext.svelte';
import { makeStubPanelContext } from '../helpers/panelContext';
import { createThreadPane } from '../../lib/stores/thread.svelte';
import {
  destroyPane,
  focusPane,
  registerPaneForTest,
  resetPanesForTest,
} from '../../lib/stores/panes.svelte';
import {
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
} from '../../lib/stores/paneLayout.svelte';
import { resetLayoutMetricsForTest } from '../../lib/stores/layoutMetrics.svelte';
import { resetComposerDraftSnapshotsForTest } from '../../lib/stores/composerDraft.svelte';
import { resetForTest as resetThreadStatuses, projectSendStarted } from '../../lib/stores/threadStatuses.svelte';
import { __resetReviewPaneStateForTest } from '../../lib/stores/reviewPane.svelte';
import { resetForTest as resetDiffReviewCommentsForTest } from '../../lib/stores/diffReviewComments.svelte';
import { resetAppStorageForTest } from '../../lib/stores/appStorage';
import {
  resetForTest as resetAccountInfoForTest,
  setProviderAccount,
} from '../../lib/stores/accountInfo.svelte';
import type { Thread } from '../../lib/types/models';
import { setBindingMock, resetBindingMocks } from '../mocks/bindings-app';
import { installPaneMocks, makeItem } from '../helpers/chat';
import type { Item } from '../../lib/types/models';
import { idleWorkspaceActivity } from '../helpers/workspaceLock';

const gc = (globalThis as { gc?: () => void }).gc;

function seedThread(id: string): Thread {
  return {
    id,
    title: 'Retention probe thread',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

async function buildPane(thread: Thread, paneId: string, items: Item[] = []) {
  installPaneMocks(items);
  setBindingMock('SwitchThread', async () => thread);
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
  setBindingMock('CountRunningBackgroundTasks', async () => 0);
  setBindingMock('GetGitStatus', async () => ({
    isRepo: false, branch: '', hasChanges: false, hasUpstream: false,
    isDefaultBranch: false, aheadCount: 0, behindCount: 0, openPrUrl: '',
    dirty: false, files: [],
  }));
  setBindingMock('GitListBranches', async () => []);
  setBindingMock('GetDraft', async (threadId: string) => ({
    threadId, content: '', attachmentIds: [], terminalChips: [],
  }));
  setBindingMock('SaveDraft', async () => {});
  setBindingMock('ListAttachments', async () => []);
  const pane = createThreadPane({ paneId });
  registerPaneForTest(paneId, pane);
  focusPane(paneId);
  await pane.switchThread(thread);
  return pane;
}

const settle = () => new Promise((r) => setTimeout(r, 25));

async function collectHard(): Promise<void> {
  for (let i = 0; i < 5; i += 1) {
    gc!();
    await settle();
  }
}

function survivors(refs: WeakRef<Element>[]): number {
  return refs.filter((r) => r.deref() !== undefined).length;
}

beforeAll(() => {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: () => unknown }).animate = function fakeAnimate() {
      return {
        finished: Promise.resolve(), currentTime: 0, playState: 'finished',
        cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
        addEventListener() {}, removeEventListener() {}, onfinish: null,
      };
    };
  }
});

beforeEach(() => {
  resetBindingMocks();
  resetPanesForTest();
  resetPaneLayoutForTest();
  resetLayoutMetricsForTest();
  resetThreadStatuses();
  resetComposerDraftSnapshotsForTest();
  resetAccountInfoForTest();
});

describe.runIf(gc)('closed chat pane DOM is collectable', () => {
  it('unmount + destroyPane releases the pane subtree (incl. composer textarea)', async () => {
    // Inner scope so no stack local keeps the elements alive.
    async function mountAndClose(): Promise<WeakRef<Element>[]> {
      const pane = await buildPane(seedThread('thread-ret-1'), 'pane-ret-1');
      // Exercise the pendingSendThreads read path so composer deriveds
      // subscribe to module signals with a present key.
      projectSendStarted('thread-ret-1');

      const target = document.body.appendChild(document.createElement('div'));
      const app = mount(ChatView, { target, props: { pane } });
      flushSync();
      await tick();
      await settle();
      await settle();

      const textarea = target.querySelector('textarea[aria-label="Message Input"]');
      const root = target.firstElementChild;
      expect(textarea, 'probe precondition: composer textarea mounted').not.toBeNull();
      const refs = [new WeakRef(textarea!), new WeakRef(root!)];

      unmount(app);
      flushSync();
      destroyPane('pane-ret-1');
      flushSync();
      target.remove();
      await settle();
      return refs;
    }

    const refs = await mountAndClose();
    await collectHard();
    expect(survivors(refs), 'pane DOM still strongly reachable after close').toBe(0);
  });
});

describe.runIf(gc)('closed review companion DOM is collectable', () => {
  it('unmounting ReviewPane (source pane still alive) releases the diff DOM', async () => {
    // Mirrors the live-app ghost scenario: the review companion closes
    // while the source thread pane stays open, so any per-thread review
    // store state keeps living. The rendered diff must still be
    // collectable.
    resetAppStorageForTest();
    __resetReviewPaneStateForTest();
    resetDiffReviewCommentsForTest();
    const diff = [
      'diff --git a/src/app.ts b/src/app.ts',
      'index 1111111..2222222 100644',
      '--- a/src/app.ts',
      '+++ b/src/app.ts',
      '@@ -1 +1 @@',
      '-old',
      '+new',
    ].join('\n');
    setBindingMock('GetThread', async () => ({ id: 'thread-rev-1', workspacePath: '/repo' }));
    setBindingMock('GetGitStatus', async () => ({}));
    setBindingMock('GetWorkspaceCurrentDiff', async () => diff);
    setBindingMock('GetBranchBaseDiff', async () => '');
    setBindingMock('ListBranchCommits', async () => []);
    setBindingMock('GetCommitDiff', async () => '');
    setBindingMock('ListPRCommits', async () => []);
    setBindingMock('GetPRCommitDiff', async () => '');
    setBindingMock('ListThreadEditDiffs', async () => ({ entries: [], turnLabels: [] }));
    setBindingMock('GetTurnEditsDiff', async () => ({ data: '' }));
    setBindingMock('GetPayloadData', async () => ({ data: '' }));
    setBindingMock('GitListBranches', async () => [{ name: 'main', isCurrent: false, isDefault: true }]);
    setBindingMock('ListDiffReviewComments', async () => []);

    const sourcePane = await buildPane(seedThread('thread-rev-1'), 'pane-rev-src');

    async function mountAndClose(): Promise<WeakRef<Element>[]> {
      const ctx: PanelContext = makeStubPanelContext({
        paneId: sourcePane.paneId,
        threadId: 'thread-rev-1',
        thread: sourcePane.thread,
      });
      const target = document.body.appendChild(document.createElement('div'));
      const app = mount(ReviewPane, { target, props: { ctx } });
      flushSync();
      await tick();
      for (let i = 0; i < 20 && !target.querySelector('[data-testid="review-line-block"]'); i += 1) {
        await settle();
      }
      const section = target.querySelector('[data-testid="review-pane"]');
      const lineBlock = target.querySelector('[data-testid="review-line-block"]');
      expect(section, 'probe precondition: review pane mounted').not.toBeNull();
      expect(lineBlock, 'probe precondition: diff rows rendered').not.toBeNull();
      const refs = [new WeakRef(section!), new WeakRef(lineBlock!)];

      unmount(app);
      flushSync();
      target.remove();
      await settle();
      return refs;
    }

    const refs = await mountAndClose();
    await collectHard();
    expect(survivors(refs), 'review pane DOM still strongly reachable after close').toBe(0);
  });
});

describe.runIf(gc)('closed pane under PaneHost is collectable while siblings live', () => {
  it('removing one pane from the layout releases its section + subtree', async () => {
    // The live-app ghost topology: real ChatViews under PaneHost (each
    // pane's ChatView receives the host's onPaneDragStart closure, whose
    // scope references the pane <section>), a sibling pane surviving
    // with live subscriptions on the same module signals, and one pane
    // closing. This is the scenario that caught the proposedPlans
    // singleton-capture leak.
    async function setupPanes(): Promise<void> {
      await buildPane(seedThread('thread-ph-a'), 'pane-ph-a');
      await buildPane(seedThread('thread-ph-b'), 'pane-ph-b');
      projectSendStarted('thread-ph-a');
      projectSendStarted('thread-ph-b');
      setPaneLayoutItemsForTest([
        { id: 'pane-ph-a', paneId: 'pane-ph-a', kind: 'thread', widthPx: 400 },
        { id: 'pane-ph-b', paneId: 'pane-ph-b', kind: 'thread', widthPx: 400 },
      ]);
    }
    await setupPanes();

    const target = document.body.appendChild(document.createElement('div'));
    const app = mount(PaneHost, { target, props: {} });
    flushSync();
    await tick();
    await settle();
    await settle();

    async function closePane(): Promise<WeakRef<Element>[]> {
      const sectionA = target.querySelector('[data-pane-id="pane-ph-a"]');
      const textareaA = sectionA?.querySelector('textarea[aria-label="Message Input"]');
      expect(sectionA, 'probe precondition: pane A section mounted').not.toBeNull();
      expect(textareaA, 'probe precondition: pane A composer mounted').not.toBeNull();
      const refs = [new WeakRef(sectionA!), new WeakRef(textareaA!)];

      setPaneLayoutItemsForTest([
        { id: 'pane-ph-b', paneId: 'pane-ph-b', kind: 'thread', widthPx: 400 },
      ]);
      flushSync();
      destroyPane('pane-ph-a');
      flushSync();
      await settle();
      return refs;
    }

    const refs = await closePane();
    await collectHard();

    // Pane B must still be alive and mounted throughout.
    expect(target.querySelector('[data-pane-id="pane-ph-b"]')).not.toBeNull();
    expect(survivors(refs), 'closed pane DOM still strongly reachable with sibling alive').toBe(0);

    unmount(app);
    flushSync();
    target.remove();
  });
});

describe.runIf(!gc)('closed chat pane DOM is collectable (SKIPPED)', () => {
  it('requires --expose-gc (run with NODE_OPTIONS=--expose-gc)', () => {
    expect(true).toBe(true);
  });
});

describe.runIf(gc)('closed pane after the rate-limit popover was re-hovered post session-connect', () => {
  it('releases the pane even though the toolbar account derived changed its dep list between hovers', async () => {
    // Live-app shape (2026-08-23 heap snapshot): the ring popover is the
    // only reader of ComposerToolbar's `sessionUsesSelectedAccount`, and
    // that derived reads the session account's proxied fields before
    // `selectedAccount`. Hover → leave (the chain disconnects) → the
    // session account is re-announced as a new object (new proxied
    // sources, so the dep list past `sessionAccount` is new next run) →
    // hover is exactly "a disconnected derived reconnects while dirty with
    // a changed dep list" — the sequence pristine svelte double-registers
    // (reconnect-dedupe hunk of patches/svelte@5.57.0.patch; focused
    // regression in svelte-patch-reconnect-dedupe.test.ts). The leftover
    // registration pinned `selectedAccount` in the global accounts signal
    // and, through the derived's closure context, the closed pane's whole
    // DOM.
    const threadId = 'thread-ring-1';
    setProviderAccount('claude', { email: 'ring@example.test', subscriptionType: 'max' }, 'acct-ring');

    async function hoverRingOnceAndLeave(root: Element, expectEmail: boolean): Promise<void> {
      const ring = root.querySelector('[data-testid="composer-rate-limit-5h"] button');
      expect(ring, 'probe precondition: rate-limit ring mounted (pane locked + provider)').not.toBeNull();
      ring!.dispatchEvent(new MouseEvent('mouseenter', { bubbles: false }));
      flushSync();
      await tick();
      const tip = document.querySelector('[role="tooltip"]');
      expect(tip, 'probe precondition: ring popover open').not.toBeNull();
      if (expectEmail) {
        expect(tip!.textContent, 'probe precondition: popover reads the session account email').toContain('ring@example.test');
      }
      ring!.dispatchEvent(new MouseEvent('mouseleave', { bubbles: false }));
      // useHoverPopover closes 140ms after leave.
      await new Promise((r) => setTimeout(r, 200));
      flushSync();
      expect(document.querySelector('[role="tooltip"]'), 'probe precondition: ring popover closed').toBeNull();
    }

    async function mountHoverAndClose(): Promise<WeakRef<Element>[]> {
      const pane = await buildPane(seedThread(threadId), 'pane-ring-1', [
        makeItem({ id: 'user:0', threadId, kind: 'user_message', role: 'user', summary: 'hi' }),
      ]);
      const target = document.body.appendChild(document.createElement('div'));
      const app = mount(ChatView, { target, props: { pane } });
      flushSync();
      await tick();
      await settle();
      await settle();

      const toolbar = target.querySelector('[data-composer-toolbar]');
      const root = target.firstElementChild;
      expect(toolbar, 'probe precondition: composer toolbar mounted').not.toBeNull();
      const refs = [new WeakRef(toolbar!), new WeakRef(root!)];

      // The session is connected on the selected account, so hover 1 runs
      // the full chain: sessionUsesSelectedAccount reads the session
      // account's proxied fields AND selectedAccount.
      const connect = () =>
        pane.setProviderSessionAccount({
          provider: 'claude',
          threadId,
          connected: true,
          accountId: 'acct-ring',
          account: { email: 'ring@example.test', subscriptionType: 'max' },
        });
      connect();
      flushSync();
      await hoverRingOnceAndLeave(target, true);
      // The session account is re-announced (every turn start does this):
      // a NEW object, so its proxied fields are new sources. The next run
      // of the disconnected derived keeps `sessionAccount` as its first dep
      // and sees everything after it — including selectedAccount — as new.
      connect();
      flushSync();
      // Hover 2: the disconnected deriveds reconnect while dirty.
      await hoverRingOnceAndLeave(target, true);

      unmount(app);
      flushSync();
      destroyPane('pane-ring-1');
      flushSync();
      target.remove();
      await settle();
      return refs;
    }

    const refs = await mountHoverAndClose();
    await collectHard();
    expect(survivors(refs), 'closed pane DOM still strongly reachable after ring re-hover').toBe(0);
  });
});
