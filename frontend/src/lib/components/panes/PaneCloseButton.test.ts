// PaneCloseButton stops a click on the X from triggering pane-level focus
// side effects. The pane section (PaneHost) focuses the pane on pointerdown
// and — when that click is a focus transition — scrolls it into view;
// without the pointerdown stop, closing an unfocused, partially-scrolled
// pane first smooth-scrolls it on-screen and then closes. The focusin stop
// matters on Chromium-engine webviews (buttons take focus on mousedown):
// without it, logical focus lands on the dying pane and destroyPane's
// dangling-focus fixup then focuses + reveals its neighbor, stealing focus
// from wherever the user was working. These tests lock both stops and the
// destroy.

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

  it('stops focusin and pointerdown from reaching the pane focus handlers', async () => {
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
