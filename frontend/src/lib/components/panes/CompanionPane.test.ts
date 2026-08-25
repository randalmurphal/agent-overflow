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

  // The 15s test timeout matches the findBy wait below: the agent body is
  // a lazily-imported chunk, and under full-suite worker load the default
  // 5s test timeout fired before the 10s findBy could (flake seen twice on
  // full runs, passes solo in ~5s).
  it('mounts the agent body at the scope its companion was opened on', { timeout: 15_000 }, async () => {
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

    // The body is a lazily-imported chunk — and no longer a tiny
    // placeholder: it pulls the chat row components. Give the first
    // paint more than findBy's 1s default.
    await findByTestId('companion-pane-agent-body', {}, { timeout: 10_000 });
    // textContent flattens inter-element whitespace, so normalize around
    // the separator glyphs before comparing.
    expect(getByTestId('agent-pane-breadcrumb').textContent?.replace(/\s*›\s*/g, ' › ').trim())
      .toBe('main › code-review › Angle B');
    expect(getByTestId('agent-pane-breadcrumb-current').textContent?.trim()).toBe('Angle B');
    // The scoped row is not in the loaded window — the body says so
    // rather than self-closing (close requires having SEEN the row).
    expect(getByTestId('agent-pane-not-loaded')).toBeTruthy();
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
