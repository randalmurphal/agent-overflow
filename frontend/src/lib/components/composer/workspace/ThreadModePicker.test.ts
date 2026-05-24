import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';

import ThreadModePicker from './ThreadModePicker.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import {
  addProjectLocal,
  resetProjectsForTest,
} from '../../../stores/projects.svelte';
import type { Project, Thread } from '../../../types/models';
import {
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';

function makeProject(overrides: Partial<Project> = {}): Project {
  return {
    id: 'project-1',
    path: '/repo',
    name: 'Project',
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/repo',
    projectPath: '/repo',
    projectId: 'project-1',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane(thread: Thread) {
  setBindingMock('SwitchThread', async () => thread);
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('ListThreadSliceAround', async () => ({
    items: [],
    oldestTurnIndex: -1,
    hasMore: false,
  }));
  setBindingMock('ListPendingInteractiveRequests', async () => ({
    approvals: [],
    userInputs: [],
  }));
  setBindingMock('GetThreadLiveState', async (threadId: string) => ({
    threadId,
    activeTurn: null,
    queueItems: [],
    interactive: { approvals: [], userInputs: [] },
    todo: null,
  }));
  setBindingMock('ListRecentTurns', async () => []);
  setBindingMock('ListThreadCheckpoints', async () => []);
  setBindingMock('AutoResumeThread', async () => null);
  // ThreadModePicker fetches fresh defaults via GetThreadDefaults
  // before swapping the placeholder so the new mode picks up the
  // seeded model / branch / effort. Tests mock a stable payload so
  // assertions on the spy don't race the binding.
  setBindingMock('GetThreadDefaults', async () => ({
    provider: 'claude',
    model: 'claude-sonnet-4-6',
    reasoningEffort: 'medium',
    fastMode: false,
    contextWindow: 200000,
    runtimeMode: 'chat',
    branch: 'main',
    workspacePath: '/repo',
  }));
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

describe('<ThreadModePicker>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetProjectsForTest();
    addProjectLocal(makeProject());
  });

  it('renders nothing when the pane has no thread', () => {
    const pane = createThreadPane();
    const { container } = render(ThreadModePicker, { props: { pane } });
    // No <button> rendered at all when there's no thread to label.
    expect(container.querySelector('[data-testid="thread-mode-picker-trigger"]')).toBeNull();
  });

  it('renders interactive trigger with chevron on a draft (isDraft=true)', async () => {
    const pane = await buildPane(makeThread({ isDraft: true }));
    const { getByTestId } = render(ThreadModePicker, { props: { pane } });
    const trigger = getByTestId('thread-mode-picker-trigger');
    expect(trigger.textContent ?? '').toMatch(/Chat/);
    expect(trigger.getAttribute('data-locked')).toBeNull();
    expect(trigger.getAttribute('aria-haspopup')).toBe('menu');
    expect(trigger).not.toBeDisabled();
  });

  it('renders interactive trigger on a placeholder (hasDraftPlaceholder=true)', async () => {
    const pane = await buildPane(makeThread());
    pane.startDraftPlaceholder(makeProject(), 'chat');
    await tick();
    const { getByTestId } = render(ThreadModePicker, { props: { pane } });
    expect(getByTestId('thread-mode-picker-trigger').getAttribute('data-locked')).toBeNull();
  });

  it('shows the Design label and icon on a design draft', async () => {
    const pane = await buildPane(makeThread({ mode: 'design', isDraft: true }));
    const { getByTestId } = render(ThreadModePicker, { props: { pane } });
    const trigger = getByTestId('thread-mode-picker-trigger');
    expect(trigger.textContent ?? '').toMatch(/Design/);
  });

  it('locks the trigger on a committed thread (no chevron, disabled)', async () => {
    // Committed = real thread row with isDraft falsy AND no placeholder. Mode
    // immutability post-creation lives in the backend; the picker reflects
    // it by going read-only.
    const pane = await buildPane(makeThread({ isDraft: false }));
    const { getByTestId } = render(ThreadModePicker, { props: { pane } });
    const trigger = getByTestId('thread-mode-picker-trigger');
    expect(trigger.getAttribute('data-locked')).toBe('true');
    expect(trigger.getAttribute('aria-haspopup')).toBeNull();
    expect(trigger).toBeDisabled();
    expect(trigger.textContent ?? '').toMatch(/Chat/);
  });

  it('clicking the trigger on a locked thread does not open the menu', async () => {
    const pane = await buildPane(makeThread({ isDraft: false }));
    const { getByTestId, queryByRole } = render(ThreadModePicker, { props: { pane } });
    await fireEvent.click(getByTestId('thread-mode-picker-trigger'));
    await tick();
    expect(queryByRole('menu')).toBeNull();
  });

  it('opens the menu on click and lists both modes with a checkmark on the current mode', async () => {
    const pane = await buildPane(makeThread({ isDraft: true }));
    const { getByTestId, findByRole } = render(ThreadModePicker, { props: { pane } });
    await fireEvent.click(getByTestId('thread-mode-picker-trigger'));
    const chatItem = await findByRole('menuitem', { name: /Chat/ });
    const designItem = await findByRole('menuitem', { name: /Design/ });
    // MenuItem renders the checked state as a unicode "✓" glyph in an
    // aria-hidden span, not via aria-checked on the menuitem itself.
    expect(chatItem.textContent ?? '').toContain('✓');
    expect(designItem.textContent ?? '').not.toContain('✓');
  });

  it('selecting Design on a chat draft flips the placeholder with seeded defaults', async () => {
    const pane = await buildPane(makeThread({ isDraft: true }));
    const spy = vi.spyOn(pane, 'startDraftPlaceholder');
    const { getByTestId, findByRole } = render(ThreadModePicker, { props: { pane } });
    await fireEvent.click(getByTestId('thread-mode-picker-trigger'));
    const designItem = await findByRole('menuitem', { name: /Design/ });
    await fireEvent.click(designItem);
    // Flip goes through flipPaneDraftPlaceholder, which awaits
    // GetThreadDefaults before calling startDraftPlaceholder — the spy
    // fires only after the binding resolves.
    await vi.waitFor(() => {
      expect(spy).toHaveBeenCalledTimes(1);
    });
    expect(spy.mock.calls[0][1]).toBe('design');
    expect(spy.mock.calls[0][0]).toMatchObject({ id: 'project-1' });
    // The seeded defaults from GetThreadDefaults flow into the
    // placeholder so the new design draft's toolbar isn't blank
    // (regression: switching to design used to drop the seeded model
    // and branch).
    expect(spy.mock.calls[0][2]).toMatchObject({
      model: 'claude-sonnet-4-6',
      branch: 'main',
    });
  });

  it('selecting the current mode is a no-op (no re-fire of startDraftPlaceholder)', async () => {
    const pane = await buildPane(makeThread({ isDraft: true }));
    const spy = vi.spyOn(pane, 'startDraftPlaceholder');
    const { getByTestId, findByRole } = render(ThreadModePicker, { props: { pane } });
    await fireEvent.click(getByTestId('thread-mode-picker-trigger'));
    const chatItem = await findByRole('menuitem', { name: /Chat/ });
    await fireEvent.click(chatItem);
    // Microtask boundary in case any async path was triggered; the
    // no-op assertion has to wait long enough for a hypothetical flip
    // call to land before declaring "didn't fire".
    await Promise.resolve();
    await Promise.resolve();
    expect(spy).not.toHaveBeenCalled();
  });
});
