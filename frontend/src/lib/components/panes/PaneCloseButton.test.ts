// PaneCloseButton stops a click on the X from triggering pane-level focus /
// reveal. The pane section (PaneHost) and chat column (ChatView) both focus —
// and thereby scroll-into-view — the pane on focusin and pointerdown. The
// button takes focus on click, so without stopping focusin, closing a
// partially-scrolled pane first smooth-scrolls it on-screen and then closes:
// a jarring shift the user reported. These tests lock both propagation stops
// (pointerdown was already handled; focusin was the gap) and the destroy.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import PaneCloseButtonHarness from './PaneCloseButtonHarness.svelte';
import { getPane, registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { createThreadPane } from '../../stores/thread.svelte';

describe('<PaneCloseButton>', () => {
  beforeEach(() => {
    resetPanesForTest();
  });

  it('stops focusin and pointerdown from reaching the pane focus/reveal handlers', async () => {
    const onAncestorFocusIn = vi.fn();
    const onAncestorPointerDown = vi.fn();
    const { getByTestId } = render(PaneCloseButtonHarness, {
      props: { paneId: 'main', onAncestorFocusIn, onAncestorPointerDown },
    });
    await tick();
    const button = getByTestId('pane-close');

    await fireEvent.pointerDown(button);
    expect(onAncestorPointerDown).not.toHaveBeenCalled();

    await fireEvent.focusIn(button);
    expect(onAncestorFocusIn).not.toHaveBeenCalled();
  });

  it('destroys the pane on click', async () => {
    const pane = createThreadPane({ paneId: 'main' });
    registerPaneForTest('main', pane);
    const { getByTestId } = render(PaneCloseButtonHarness, {
      props: { paneId: 'main' },
    });
    await tick();
    expect(getPane('main')).toBeDefined();

    await fireEvent.click(getByTestId('pane-close'));
    expect(getPane('main')).toBeUndefined();
  });
});
