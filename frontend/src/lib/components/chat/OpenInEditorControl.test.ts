import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import OpenInEditorControl from './OpenInEditorControl.svelte';
import {
  ensureEditorsLoaded,
  resetEditorsForTest,
} from '../../stores/editors.svelte';
import {
  setBindingMock,
  resetBindingMocks,
} from '../../../test/mocks/bindings-app';
import type { EditorInfo } from '../../stores/bindings';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';

// Popover/Menu transitions poke Element.animate on mount; happy-dom lacks it.
if (typeof Element !== 'undefined' && !('animate' in Element.prototype)) {
  (Element.prototype as unknown as { animate: unknown }).animate = function () {
    return {
      cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
      addEventListener() {}, removeEventListener() {},
      onfinish: null, oncancel: null, finished: Promise.resolve(),
      effect: null, startTime: 0, currentTime: 0, playState: 'finished', playbackRate: 1,
    };
  };
}

function ed(id: string, name: string, available = true, envFallback = false): EditorInfo {
  return { id, name, available, envFallback } as EditorInfo;
}

// Seed the shared store before mount so the derived icon/dropdown are
// settled on first render (the component's own $effect would load it
// too, but pre-loading removes the async race from these assertions).
async function seed(editors: EditorInfo[], preference = ''): Promise<void> {
  setBindingMock('ListAvailableEditors', vi.fn(async () => editors));
  setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference })));
  await ensureEditorsLoaded();
}

describe('<OpenInEditorControl>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetEditorsForTest();
    setPageGrantsFromBootstrap(false);
  });

  afterEach(() => {
    setPageGrantsFromBootstrap(false);
  });

  it('opens in the default editor and shows no dropdown with a single editor', async () => {
    await seed([ed('code', 'Visual Studio Code')]);
    const open = setBindingMock('OpenInEditor', vi.fn(async () => undefined));

    const { getByTestId, queryByTestId } = render(OpenInEditorControl, {
      props: { path: '/proj', name: 'Alpha' },
    });

    expect(queryByTestId('chat-header-open-editor-caret')).toBeNull();

    await fireEvent.click(getByTestId('chat-header-open-editor'));
    await waitFor(() => expect(open).toHaveBeenCalledTimes(1));
    // Empty editorID → backend resolves the saved default.
    expect(open.mock.calls[0]).toEqual(['/proj', 0, 0, '', '']);
  });

  it('shows a dropdown and opens a chosen editor one-shot without changing the default', async () => {
    await seed([ed('code', 'Visual Studio Code'), ed('cursor', 'Cursor'), ed('zed', 'Zed')]);
    const open = setBindingMock('OpenInEditor', vi.fn(async () => undefined));

    const { getByTestId, getByText, findByText } = render(OpenInEditorControl, {
      props: { path: '/proj', name: 'Alpha' },
    });

    // Caret present because more than one editor is available.
    await fireEvent.click(getByTestId('chat-header-open-editor-caret'));

    // Every available editor is offered.
    await findByText('Cursor');
    expect(getByText('Visual Studio Code')).toBeTruthy();
    expect(getByText('Zed')).toBeTruthy();

    // Picking a non-default editor opens in exactly that one (its id),
    // not the empty-string default.
    const cursorItem = getByText('Cursor').closest('[role="menuitem"]');
    await fireEvent.click(cursorItem!);
    await waitFor(() => expect(open).toHaveBeenCalledTimes(1));
    expect(open.mock.calls[0]).toEqual(['/proj', 0, 0, '', 'cursor']);
  });

  it('marks the resolved default in the dropdown with a check', async () => {
    // Preference points at Cursor, so it is the default; its row carries
    // the check while the others do not.
    await seed([ed('code', 'Visual Studio Code'), ed('cursor', 'Cursor')], 'cursor');
    setBindingMock('OpenInEditor', vi.fn(async () => undefined));

    const { getByTestId, getByText, findByText } = render(OpenInEditorControl, {
      props: { path: '/proj', name: 'Alpha' },
    });

    await fireEvent.click(getByTestId('chat-header-open-editor-caret'));
    await findByText('Cursor');

    const cursorItem = getByText('Cursor').closest('[role="menuitem"]');
    const codeItem = getByText('Visual Studio Code').closest('[role="menuitem"]');
    expect(cursorItem?.textContent).toContain('✓');
    expect(codeItem?.textContent).not.toContain('✓');
  });

  it('still renders a working button when no editor is available', async () => {
    await seed([]);
    const open = setBindingMock('OpenInEditor', vi.fn(async () => undefined));

    const { getByTestId, queryByTestId } = render(OpenInEditorControl, {
      props: { path: '/proj', name: 'Alpha' },
    });

    expect(queryByTestId('chat-header-open-editor-caret')).toBeNull();
    // Clicking still hits the backend, which surfaces the real
    // "no editor available" error to the user.
    await fireEvent.click(getByTestId('chat-header-open-editor'));
    await waitFor(() => expect(open).toHaveBeenCalledTimes(1));
    expect(open.mock.calls[0]).toEqual(['/proj', 0, 0, '', '']);
  });

  it('renders no editor control and makes no editor RPC in a view-only session', async () => {
    setPageGrantsFromBootstrap(true);
    const list = setBindingMock('ListAvailableEditors', vi.fn(async () => []));
    const settings = setBindingMock('GetEditorSettings', vi.fn(async () => ({ preference: '' })));
    const open = setBindingMock('OpenInEditor', vi.fn(async () => undefined));

    const { queryByTestId } = render(OpenInEditorControl, {
      props: { path: '/proj', name: 'Alpha' },
    });
    await Promise.resolve();

    expect(queryByTestId('chat-header-open-editor')).toBeNull();
    expect(list).not.toHaveBeenCalled();
    expect(settings).not.toHaveBeenCalled();
    expect(open).not.toHaveBeenCalled();
  });
});
