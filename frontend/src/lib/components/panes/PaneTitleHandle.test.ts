// PaneTitleHandle's rename / drag / focus-ring behavior is exercised
// end-to-end by ChatHeader.test (chat header) and TerminalView.test (terminal
// header), both of which render the real component. This file locks the parts
// those don't touch: the parameterized testids that let each pane surface keep
// its own identity, and the no-thread gate.

import { beforeEach, describe, expect, it } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import PaneTitleHandle from './PaneTitleHandle.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import { resetProjectsForTest } from '../../stores/projects.svelte';
import { resetSidebarForTest } from '../../stores/sidebar.svelte';
import type { Thread } from '../../types/models';
import { buildPane as buildRegisteredPane, makeThread as makeBaseThread } from '../../../test/helpers/chat';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return makeBaseThread({
    title: 'Title',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/proj',
    projectId: 'project-1',
    ...overrides,
  });
}

async function buildPane(thread: Thread = makeThread(), paneId = 'main') {
  return buildRegisteredPane(thread, [], paneId);
}

describe('<PaneTitleHandle>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetProjectsForTest();
    resetSidebarForTest();
    resetPanesForTest();
  });

  it('defaults to the pane-title testid and renders the thread title as a button', async () => {
    const pane = await buildPane(makeThread({ title: 'Defaulted' }));
    const { getByTestId } = render(PaneTitleHandle, { props: { pane } });
    await tick();
    const title = getByTestId('pane-title');
    expect(title.tagName.toLowerCase()).toBe('button');
    expect(title).toHaveTextContent('Defaulted');
  });

  it('lets a consumer override the title/input testids', async () => {
    const pane = await buildPane();
    const { getByTestId, queryByTestId } = render(PaneTitleHandle, {
      props: { pane, titleTestId: 'custom-title', inputTestId: 'custom-input' },
    });
    await tick();
    expect(getByTestId('custom-title')).toBeInTheDocument();
    // The default id must not leak when overridden.
    expect(queryByTestId('pane-title')).toBeNull();
  });

  it('renders nothing when the pane has no thread', async () => {
    const pane = createThreadPane({ paneId: 'empty' });
    registerPaneForTest('empty', pane);
    const { queryByTestId } = render(PaneTitleHandle, { props: { pane } });
    await tick();
    expect(queryByTestId('pane-title')).toBeNull();
  });

  it('keeps an open rename alive across a same-id thread reassignment, cancels on a real swap', async () => {
    // events.ts bumps pane.thread on activity/token-usage/status via
    // replaceThread({ ...thread, … }) — same id, new object. Reading
    // pane.thread.id subscribes the reset effect to that field, so without the
    // id-change guard those same-id reassignments would re-run it and drop the
    // user's in-flight draft. The reset must fire ONLY on a real id change.
    const pane = await buildPane(makeThread({ id: 'thread-1', title: 'Original' }));
    const { getByTestId, queryByTestId } = render(PaneTitleHandle, { props: { pane } });
    await tick();

    // Open the inline rename (right-click) and type a draft.
    await fireEvent.contextMenu(getByTestId('pane-title'));
    await tick();
    await fireEvent.input(getByTestId('pane-title-input'), { target: { value: 'My Draft' } });

    // A same-id activity bump must leave the edit (and the draft) intact.
    pane.replaceThread({ ...pane.thread!, updatedAt: 999 });
    await tick();
    const stillEditing = getByTestId('pane-title-input') as HTMLInputElement;
    expect(stillEditing).toBeInTheDocument();
    expect(stillEditing.value).toBe('My Draft');

    // An actual thread swap (different id) cancels the rename.
    pane.replaceThread(makeThread({ id: 'thread-2', title: 'Other' }));
    await tick();
    expect(queryByTestId('pane-title-input')).toBeNull();
    expect(getByTestId('pane-title')).toHaveTextContent('Other');
  });
});
