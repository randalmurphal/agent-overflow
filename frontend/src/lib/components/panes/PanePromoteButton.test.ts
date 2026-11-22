// The pane header's maximize control and the title double-click: both
// promote the pane (focusing it, so focus moving elsewhere does not demote
// it at once) and restore it when it is the promoted pane. The button's
// label, icon and title follow the state and name the shortcut. Compact
// shows one pane already, so neither does anything there.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import PanePromoteButton from './PanePromoteButton.svelte';
import PaneTitleHandle from './PaneTitleHandle.svelte';
import { focusPane, getFocusedPaneId, registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { getPromotedPaneId, resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { resetKeybindingsStore, setKeybindingsForTest } from '../../stores/keybindings.svelte';
import { makeThread } from '../../../test/helpers/chat';

describe('pane promote controls', () => {
  beforeEach(() => {
    resetPanesForTest();
    resetPaneLayoutForTest();
    registerPaneForTest('a', createThreadPane({ paneId: 'a' }));
    registerPaneForTest('b', createThreadPane({ paneId: 'b' }));
    setPaneLayoutItemsForTest([
      { id: 'a', paneId: 'a', kind: 'thread', widthPx: 500 },
      { id: 'b', paneId: 'b', kind: 'thread', widthPx: 500 },
    ]);
    focusPane('a');
    setKeybindingsForTest([{ key: 'alt+shift+enter', command: 'pane.togglePromoted' }]);
  });

  afterEach(() => {
    setCompactLayoutForTest(false);
    resetKeybindingsStore();
    resetPaneLayoutForTest();
    resetPanesForTest();
  });

  it('the button focuses and promotes its pane, then restores it, naming the shortcut', async () => {
    const { getByTestId } = render(PanePromoteButton, { props: { paneId: 'b' } });
    const button = getByTestId('pane-promote');
    expect(button.getAttribute('aria-label')).toBe('Maximize Pane');
    expect(button.getAttribute('title')).toMatch(/^Maximize Pane \(.+\)$/);

    await fireEvent.click(button);
    expect(getPromotedPaneId()).toBe('b');
    expect(getFocusedPaneId()).toBe('b');
    await tick();
    expect(button.getAttribute('aria-label')).toBe('Restore Pane');
    expect(button.getAttribute('title')).toMatch(/^Restore Pane \(.+\)$/);

    await fireEvent.click(button);
    expect(getPromotedPaneId()).toBeNull();
    await tick();
    expect(button.getAttribute('aria-label')).toBe('Maximize Pane');
  });

  it('names no shortcut when the command is unbound', () => {
    setKeybindingsForTest([]);
    const { getByTestId } = render(PanePromoteButton, { props: { paneId: 'b' } });
    expect(getByTestId('pane-promote').getAttribute('title')).toBe('Maximize Pane');
  });

  it('renders nothing under compact', () => {
    setCompactLayoutForTest(true);
    const { queryByTestId } = render(PanePromoteButton, { props: { paneId: 'b' } });
    expect(queryByTestId('pane-promote')).toBeNull();
  });

  it('a double-click on the pane title promotes and restores, except under compact', async () => {
    const pane = createThreadPane({ paneId: 'b' });
    pane.replaceThread(makeThread({ id: 't-b', title: 'Bee' }));
    registerPaneForTest('b', pane);
    const { getByTestId } = render(PaneTitleHandle, { props: { pane } });
    const title = getByTestId('pane-title');
    expect(title.getAttribute('title')).toContain('double-click to maximize');

    await fireEvent.dblClick(title);
    expect(getPromotedPaneId()).toBe('b');
    await tick();
    expect(title.getAttribute('title')).toContain('double-click to restore');
    await fireEvent.dblClick(title);
    expect(getPromotedPaneId()).toBeNull();

    setCompactLayoutForTest(true);
    await fireEvent.dblClick(title);
    expect(getPromotedPaneId()).toBeNull();
  });
});
