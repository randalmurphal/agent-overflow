import { cleanup, render } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import CompanionPane from './CompanionPane.svelte';
import { installPaneMocks, installThreadSwitchMocks, makeItem, makeThread } from '../../../test/helpers/chat';
import { createThreadPane } from '../../stores/thread.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { isCompanionOpen, openCompanion, resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { __resetReviewPaneStateForTest } from '../../stores/reviewPane.svelte';
import { __resetAgentPaneStateForTest, openAgentCompanion } from '../../stores/agentPane.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

describe('CompanionPane across a source-pane thread switch', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetCompanionPanesForTest();
    __resetReviewPaneStateForTest();
    __resetAgentPaneStateForTest();
    // Review-state creation kicks off a workspace-scope diff load; give
    // it the minimal backend surface so mount side effects resolve.
    setBindingMock('GetWorkspaceCurrentDiff', async () => '');
    setBindingMock('GetGitStatus', async () => ({}));
    setBindingMock('GetThread', async () => makeThread());
    setBindingMock('GitListBranches', async () => []);
  });

  afterEach(() => {
    cleanup();
    __resetAgentPaneStateForTest();
  });

  it('mounts the agent body at the scope its companion was opened on', async () => {
    const thread = makeThread({ id: 'thread-agent' });
    installPaneMocks([makeItem({ threadId: 'thread-agent' })]);
    const pane = createThreadPane({ paneId: 'main' });
    registerPaneForTest('main', pane);
    await pane.switchThread(thread);
    setPaneLayoutItemsForTest([
      { id: 'main', paneId: 'main', kind: 'thread', widthPx: 400 },
    ]);
    const agent = openAgentCompanion('main', 'thread-agent', 'launch-1', 'code-review');
    agent?.pushScope('launch-2', 'Angle B');

    const { findByTestId, getByTestId } = render(CompanionPane, {
      props: { paneId: 'agent-main', kind: 'agent', sourcePaneId: 'main' },
    });

    // The body is a lazily-imported chunk: await its first paint.
    await findByTestId('companion-pane-agent-body');
    expect(getByTestId('agent-pane-breadcrumb').textContent?.trim())
      .toBe('main › code-review › Angle B');
    expect(getByTestId('agent-pane-scope').textContent?.trim()).toBe('launch-2');
    expect(getByTestId('companion-pane-agent').getAttribute('aria-label')).toBe('Agent');
  });

  // Regression: with a review companion open, switching the source pane to
  // another thread used to re-evaluate the mounted ReviewPane's review
  // state against the NEW thread id before the {#key} teardown. That
  // disposed the replaced state inside a $derived — a state_unsafe_mutation
  // crash that aborted the render flush and left the chat pane blank.
  it('switching the source thread closes the companion without crashing the flush', async () => {
    const threadA = makeThread({ id: 'thread-a' });
    installPaneMocks([makeItem({ threadId: 'thread-a' })]);
    setBindingMock('SwitchThread', async () => threadA);
    // buildPane's registry key and the pane's internal paneId differ; the
    // companion close keys on the pane's OWN id, so construct it as 'main'.
    const pane = createThreadPane({ paneId: 'main' });
    registerPaneForTest('main', pane);
    await pane.switchThread(threadA);
    setPaneLayoutItemsForTest([
      { id: 'main', paneId: 'main', kind: 'thread', widthPx: 400 },
    ]);
    openCompanion('main', 'review');

    render(CompanionPane, {
      props: { paneId: 'review-main', kind: 'review', sourcePaneId: 'main' },
    });
    await tick();

    const threadB = makeThread({ id: 'thread-b' });
    installThreadSwitchMocks(threadB, []);
    // Kick off the switch, then force the render flush synchronously so a
    // crash inside it surfaces here instead of as an unhandled rejection.
    const switching = pane.switchThread(threadB);
    expect(() => flushSync()).not.toThrow();
    await switching;
    await tick();

    expect(pane.threadId).toBe('thread-b');
    expect(isCompanionOpen('main', 'review')).toBe(false);
  });
});
