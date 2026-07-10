import { cleanup, render, screen } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('./TakeControlTerminal.svelte', async () => ({
  default: (await import('../../../test/mocks/StubTakeControlTerminal.svelte')).default,
}));

import TakeControlPane from './TakeControlPane.svelte';
import { installPaneMocks, makeThread } from '../../../test/helpers/chat';
import { createThreadPane } from '../../stores/thread.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { isCompanionOpen, openCompanion, resetCompanionPanesForTest } from '../../stores/companionPanes.svelte';
import { setBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';

describe('TakeControlPane', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetCompanionPanesForTest();
  });

  afterEach(() => {
    cleanup();
  });

  async function mountWithClaudeTuiThread() {
    const threadA = makeThread({ id: 'thread-a', provider: 'claude-tui' });
    installPaneMocks([]);
    setBindingMock('SwitchThread', async () => threadA);
    const pane = createThreadPane({ paneId: 'main' });
    registerPaneForTest('main', pane);
    await pane.switchThread(threadA);
    setPaneLayoutItemsForTest([
      { id: 'main', paneId: 'main', kind: 'thread', widthPx: 400 },
    ]);
    openCompanion('main', 'take-control');
    render(TakeControlPane, { props: { paneId: 'take-control-main' } });
    await tick();
    return pane;
  }

  it('mirrors the claude-tui thread it was opened for', async () => {
    await mountWithClaudeTuiThread();

    const terminal = screen.getByTestId('stub-take-control-terminal');
    expect(terminal.getAttribute('data-thread-id')).toBe('thread-a');
    expect(isCompanionOpen('main', 'take-control')).toBe(true);
  });

  // Regression: the terminal used to "follow" a claude-tui → claude-tui
  // thread switch, silently re-attaching the mirror (and the user's
  // keystrokes) to the incoming thread's PTY. The mirrored thread is now
  // pinned at open: the switch closes the companion, and the mounted
  // surface never renders against the new thread.
  it('never re-attaches to the switched-to thread', async () => {
    const pane = await mountWithClaudeTuiThread();

    const threadB = makeThread({ id: 'thread-b', provider: 'claude-tui' });
    setBindingMock('SwitchThread', async () => threadB);
    const switching = pane.switchThread(threadB);
    expect(() => flushSync()).not.toThrow();
    await switching;
    await tick();

    expect(pane.threadId).toBe('thread-b');
    expect(isCompanionOpen('main', 'take-control')).toBe(false);
    const terminal = screen.queryByTestId('stub-take-control-terminal');
    expect(terminal?.getAttribute('data-thread-id') ?? null).not.toBe('thread-b');
  });
});
