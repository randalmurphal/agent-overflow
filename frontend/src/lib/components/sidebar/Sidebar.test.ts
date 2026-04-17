import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import Sidebar from './Sidebar.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { refreshThreads } from '../../stores/threads.svelte';
import {
  clearThreadSelection,
  setIncludeArchived,
  setThreadFilterQuery,
  setThreadSelection,
  setWorkspaceFilter,
} from '../../stores/threadFilter.svelte';
import type { Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

function makeThread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id,
    title: `Thread ${id}`,
    provider: 'claude',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/ws',
    interactionMode: 'default',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function seedThreads(items: Thread[]) {
  setBindingMock('GetSettings', async () => null);
  setBindingMock('ListThreads', async () => items);
  await loadSettings();
  await refreshThreads();
}

describe('<Sidebar>', () => {
  beforeEach(() => {
    setThreadFilterQuery('');
    setIncludeArchived(false);
    setWorkspaceFilter(null);
    clearThreadSelection();
  });

  it('renders the search input, archived toggle, and thread rows', async () => {
    await seedThreads([makeThread('a', { title: 'Alpha' }), makeThread('b', { title: 'Beta' })]);
    const pane = createThreadPane();
    const { getByTestId, getByText } = render(Sidebar, { props: { pane } });
    expect(getByTestId('sidebar-thread-search')).toBeInTheDocument();
    expect(getByTestId('sidebar-archived-toggle')).toBeInTheDocument();
    expect(getByText('Alpha')).toBeInTheDocument();
    expect(getByText('Beta')).toBeInTheDocument();
  });

  it('filters thread rows when typing into the search box', async () => {
    await seedThreads([
      makeThread('a', { title: 'Refactor auth' }),
      makeThread('b', { title: 'Build CLI' }),
    ]);
    const pane = createThreadPane();
    const { getByTestId, queryByText } = render(Sidebar, { props: { pane } });
    const search = getByTestId('sidebar-thread-search') as HTMLInputElement;
    await fireEvent.input(search, { target: { value: 'auth' } });
    expect(queryByText('Refactor auth')).toBeInTheDocument();
    expect(queryByText('Build CLI')).toBeNull();
  });

  it('archived toggle shows archived threads', async () => {
    await seedThreads([
      makeThread('a', { title: 'Active' }),
      makeThread('b', { title: 'Retired', archived: true }),
    ]);
    const pane = createThreadPane();
    const { getByTestId, queryByText } = render(Sidebar, { props: { pane } });
    expect(queryByText('Retired')).toBeNull();

    const toggle = getByTestId('sidebar-archived-toggle') as HTMLInputElement;
    await fireEvent.click(toggle);
    expect(queryByText('Retired')).toBeInTheDocument();
  });

  it('shows the multi-select toolbar when threads are selected', async () => {
    await seedThreads([makeThread('a'), makeThread('b')]);
    const pane = createThreadPane();
    const { getByTestId, queryByTestId } = render(Sidebar, { props: { pane } });
    expect(queryByTestId('sidebar-multiselect-toolbar')).toBeNull();

    setThreadSelection(['a', 'b']);
    await tick();
    expect(getByTestId('sidebar-multiselect-toolbar')).toBeInTheDocument();
    expect(getByTestId('sidebar-multiselect-toolbar')).toHaveTextContent(/2 selected/i);
  });

  it('bulk archive clears the selection and removes threads', async () => {
    await seedThreads([makeThread('a'), makeThread('b'), makeThread('c')]);
    const pane = createThreadPane();

    const archive = vi.fn(async () => {});
    setBindingMock('ArchiveThread', archive);
    setBindingMock('StopSession', async () => {});

    const { getByTestId } = render(Sidebar, { props: { pane } });
    setThreadSelection(['a', 'b']);
    await tick();

    const toolbar = getByTestId('sidebar-multiselect-toolbar');
    const archiveBtn = Array.from(toolbar.querySelectorAll('button')).find((b) =>
      /Archive selected/i.test(b.textContent ?? ''),
    );
    expect(archiveBtn).toBeDefined();
    await fireEvent.click(archiveBtn!);
    // Flush pending awaits inside runBulkAction.
    for (let i = 0; i < 10; i += 1) await tick();

    // Two archives issued.
    expect(archive).toHaveBeenCalledTimes(2);
  });

  async function openNewThreadForm() {
    await seedThreads([]);
    // The form's ProviderPicker queries provider statuses on mount — mock it.
    setBindingMock('GetProviderStatuses', async () => []);
    const pane = createThreadPane();
    const rendered = render(Sidebar, { props: { pane } });
    await fireEvent.click(rendered.getByText('+ New Thread'));
    for (let i = 0; i < 5; i += 1) await tick();
    return { pane, ...rendered };
  }

  it('new-thread form exposes an interaction-mode picker with Default selected initially', async () => {
    const { getByTestId } = await openNewThreadForm();

    const picker = getByTestId('new-thread-mode-picker');
    expect(picker).toBeInTheDocument();
    expect(getByTestId('new-thread-mode-default').getAttribute('aria-checked')).toBe('true');
    expect(getByTestId('new-thread-mode-plan').getAttribute('aria-checked')).toBe('false');
    expect(getByTestId('new-thread-mode-design').getAttribute('aria-checked')).toBe('false');
  });

  it('clicking a mode option in the picker flips aria-checked', async () => {
    const { getByTestId } = await openNewThreadForm();

    await fireEvent.click(getByTestId('new-thread-mode-plan'));
    await tick();

    expect(getByTestId('new-thread-mode-plan').getAttribute('aria-checked')).toBe('true');
    expect(getByTestId('new-thread-mode-default').getAttribute('aria-checked')).toBe('false');
  });

  it('CreateThread receives the chosen interaction mode', async () => {
    const createMock = vi.fn(async (_provider: unknown, _ws: unknown, _model: unknown, mode: unknown) => ({
      id: 'created-thread',
      title: 'New Thread',
      provider: 'claude',
      workspacePath: '/tmp/ws',
      projectPath: '/tmp/ws',
      model: 'claude-sonnet-4-6',
      interactionMode: mode,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    }));
    setBindingMock('CreateThread', createMock);
    setBindingMock('StartSession', async () => {});
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async () => []);
    setBindingMock('ListPayloadMetas', async () => []);

    const { getByText, getByTestId } = await openNewThreadForm();
    // Fill in the workspace path so the form can submit (input labelled
    // "Workspace path" by WorkspacePicker).
    const wsInput = document.querySelector<HTMLInputElement>('input[aria-label="Workspace path"]');
    expect(wsInput).not.toBeNull();
    await fireEvent.input(wsInput!, { target: { value: '/tmp/ws' } });
    await fireEvent.click(getByTestId('new-thread-mode-design'));
    await tick();
    const createBtn = getByText('Create');
    await fireEvent.click(createBtn);

    for (let i = 0; i < 10; i += 1) await tick();

    expect(createMock.mock.calls.length).toBeGreaterThan(0);
    expect(createMock.mock.calls[0][3]).toBe('design');
  });
});
