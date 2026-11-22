// Take control as the header's button and its menu row both define it:
// what takeControlState reports, and that toggleTakeControl opens the
// paired pane only for a thread that offers it, with the terminal grant
// and a persisted row.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { takeControlState, toggleTakeControl } from './headerPaneActions';
import { companionForSource, installCompanionPanes, resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { createThreadPane, type ThreadPane } from '../../stores/thread.svelte';
import { resetToastsForTest, getToasts } from '../../stores/toast.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { OBSERVE_SCOPES, pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';

function inLayout(pane: ThreadPane): void {
  setPaneLayoutItemsForTest([{ id: pane.paneId, paneId: pane.paneId, kind: 'thread', widthPx: 640 }]);
}

const takeControlPane = (pane: ThreadPane) => companionForSource(pane.paneId, 'take-control');

describe('take control', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetCompanionPanesForTest();
    installCompanionPanes();
    resetToastsForTest();
  });

  afterEach(() => {
    resetCompanionPanesForTest();
    resetPaneLayoutForTest();
    resetToLocalPage();
    resetToastsForTest();
  });

  it('opens and closes the paired pane for a claude-tui thread, and reports it open', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude-tui' }));
    inLayout(pane);
    expect(takeControlState(pane)).toEqual({ offered: true, grant: 'granted', open: false });

    toggleTakeControl(pane);
    expect(takeControlPane(pane)).not.toBeNull();
    expect(takeControlState(pane).open).toBe(true);

    toggleTakeControl(pane);
    expect(takeControlPane(pane)).toBeNull();
    expect(takeControlState(pane).open).toBe(false);
  });

  it('does nothing for a thread that does not offer it', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude' }));
    inLayout(pane);
    expect(takeControlState(pane).offered).toBe(false);
    toggleTakeControl(pane);
    expect(takeControlPane(pane)).toBeNull();
    expect(getToasts()).toEqual([]);
  });

  it('does nothing without terminal:operate', async () => {
    await pairWithScopes([...OBSERVE_SCOPES, 'threads:operate']);
    const pane = await buildPane(makeThread({ provider: 'claude-tui' }));
    inLayout(pane);
    expect(takeControlState(pane)).toMatchObject({ offered: true, grant: 'refused' });
    toggleTakeControl(pane);
    expect(takeControlPane(pane)).toBeNull();
  });

  it('asks for the thread to be started on a draft', () => {
    const pane = createThreadPane({ paneId: 'draft-take-control' });
    pane.startDraftPlaceholder(
      { id: 'project-1', path: '/tmp/workspace', name: 'Alpha', sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false },
      'chat',
      { provider: 'claude-tui', model: 'm' },
    );
    inLayout(pane);
    expect(pane.threadId).toBeNull();
    toggleTakeControl(pane);
    expect(takeControlPane(pane)).toBeNull();
    expect(getToasts().map((toast) => toast.message)).toEqual(['Start the thread before opening this panel.']);
  });
});
