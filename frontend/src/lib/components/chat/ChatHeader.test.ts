// ChatHeader tests cover the rewrite's contract: provider chip, title
// (view + inline rename), project badge, git actions, and the two
// toggles (Diffs + Plans). The old InteractionModeBadge +
// ModelPicker / RuntimeModePicker / BranchToolbar have been deleted;
// those covers are carried in the composer/below-bar tests now.

import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ChatHeader from './ChatHeader.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { setBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import {
  addProjectLocal,
  resetProjectsForTest,
} from '../../stores/projects.svelte';
import { resetSidebarForTest, isProjectExpanded } from '../../stores/sidebar.svelte';
import type { Thread } from '../../types/models';

beforeAll(() => {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        return {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
          addEventListener() {}, removeEventListener() {},
          onfinish: null, oncancel: null,
        };
      };
  }
});

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'My awesome thread',
    provider: 'claude',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/proj',
    projectId: 'project-1',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane(thread: Thread = makeThread()) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  // GitActionsControl calls GetGitStatus on mount; keep it a no-repo
  // so that component renders nothing and doesn't distract the tests.
  setBindingMock('GetGitStatus', async () => ({
    isRepo: false,
    branch: '',
    hasChanges: false,
    hasUpstream: false,
    isDefaultBranch: false,
    aheadCount: 0,
    behindCount: 0,
    openPrUrl: '',
    dirty: false,
    files: [],
  }));
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

describe('<ChatHeader>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetProjectsForTest();
    resetSidebarForTest();
  });

  it('renders the provider chip with the right letter + tint for Claude', async () => {
    const pane = await buildPane(makeThread({ provider: 'claude' }));
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    const chip = getByTestId('chat-header-provider');
    expect(chip.textContent?.trim()).toBe('C');
    expect(chip.className).toContain('text-accent');
  });

  it('renders the provider chip with X + codex tint for Codex threads', async () => {
    const pane = await buildPane(makeThread({ provider: 'codex' }));
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    const chip = getByTestId('chat-header-provider');
    expect(chip.textContent?.trim()).toBe('X');
    expect(chip.className).toContain('provider-codex');
  });

  it('shows the thread title as a button in view mode', async () => {
    const pane = await buildPane(makeThread({ title: 'Shipping design' }));
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    const title = getByTestId('chat-header-title');
    expect(title.textContent?.trim()).toBe('Shipping design');
    expect(title.tagName.toLowerCase()).toBe('button');
  });

  it('switches to an input on title click and commits the rename on Enter', async () => {
    const pane = await buildPane(makeThread({ title: 'Old name' }));
    // RenameThread resolves to void; ChatHeader then re-reads via GetThread.
    const rename = setBindingMock('RenameThread', async () => undefined);
    setBindingMock('GetThread', async () => ({
      ...(pane.thread as Thread),
      title: 'New name',
    }));
    const { getByTestId, queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();

    await fireEvent.click(getByTestId('chat-header-title'));
    await tick();
    const input = getByTestId('chat-header-title-input') as HTMLInputElement;
    expect(input.value).toBe('Old name');

    // Clear and type a new name.
    await fireEvent.input(input, { target: { value: 'New name' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    for (let i = 0; i < 4; i += 1) await Promise.resolve();

    expect(rename.mock.calls[0]).toEqual(['thread-1', 'New name']);
    expect(queryByTestId('chat-header-title-input')).toBeNull();
    expect(pane.thread?.title).toBe('New name');
  });

  it('cancels the rename when Escape is pressed without calling the binding', async () => {
    const pane = await buildPane(makeThread({ title: 'Keep me' }));
    const rename = setBindingMock('RenameThread', async () => pane.thread as Thread);
    const { getByTestId, queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    await fireEvent.click(getByTestId('chat-header-title'));
    await tick();
    const input = getByTestId('chat-header-title-input') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'Something else' } });
    await fireEvent.keyDown(input, { key: 'Escape' });
    await tick();

    expect(queryByTestId('chat-header-title-input')).toBeNull();
    expect(rename.mock.calls).toHaveLength(0);
    // Pane still shows the original title.
    expect(pane.thread?.title).toBe('Keep me');
  });

  it('skips the RenameThread call when the draft is empty or unchanged', async () => {
    const pane = await buildPane(makeThread({ title: 'Same' }));
    const rename = setBindingMock('RenameThread', async () => pane.thread as Thread);
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();

    // Empty submit.
    await fireEvent.click(getByTestId('chat-header-title'));
    await tick();
    let input = getByTestId('chat-header-title-input') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: '' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    await tick();

    // Unchanged submit.
    await fireEvent.click(getByTestId('chat-header-title'));
    await tick();
    input = getByTestId('chat-header-title-input') as HTMLInputElement;
    await fireEvent.keyDown(input, { key: 'Enter' });
    await tick();

    expect(rename.mock.calls).toHaveLength(0);
  });

  it('surfaces the error on the pane when RenameThread rejects', async () => {
    const pane = await buildPane(makeThread({ title: 'Old' }));
    setBindingMock('RenameThread', async () => {
      throw new Error('backend said no');
    });
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    await fireEvent.click(getByTestId('chat-header-title'));
    await tick();
    const input = getByTestId('chat-header-title-input') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'Tries to rename' } });
    await fireEvent.keyDown(input, { key: 'Enter' });
    for (let i = 0; i < 5; i += 1) await Promise.resolve();

    expect(pane.generalError).toMatch(/Failed to rename thread/);
    consoleErr.mockRestore();
  });

  it('renders the project badge when the projects store has the thread project', async () => {
    const now = Date.now();
    addProjectLocal({
      id: 'project-1',
      path: '/tmp/proj',
      name: 'Alpha',
      sortPosition: 0,
      createdAt: now,
      updatedAt: now,
      archived: false,
    });
    const pane = await buildPane();
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    const badge = getByTestId('chat-header-project');
    expect(badge.textContent?.trim()).toBe('Alpha');
  });

  it('hides the project badge when the thread has no projectId', async () => {
    const pane = await buildPane(makeThread({ projectId: undefined }));
    const { queryByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(queryByTestId('chat-header-project')).toBeNull();
  });

  it('expands the project in the sidebar when the badge is clicked', async () => {
    const now = Date.now();
    addProjectLocal({
      id: 'project-1',
      path: '/tmp/proj',
      name: 'Alpha',
      sortPosition: 0,
      createdAt: now,
      updatedAt: now,
      archived: false,
    });
    const pane = await buildPane();
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(isProjectExpanded('project-1')).toBe(false);
    await fireEvent.click(getByTestId('chat-header-project'));
    expect(isProjectExpanded('project-1')).toBe(true);
  });

  it('toggles the diff panel via the Diffs button', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(pane.diffPanel.open).toBe(false);
    await fireEvent.click(getByTestId('diff-panel-toggle'));
    expect(pane.diffPanel.open).toBe(true);
  });

  it('toggles the plan sidebar via the Plans button', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ChatHeader, { props: { pane } });
    await tick();
    expect(pane.showPlanSidebar).toBe(false);
    await fireEvent.click(getByTestId('plan-sidebar-toggle'));
    expect(pane.showPlanSidebar).toBe(true);
  });
});
