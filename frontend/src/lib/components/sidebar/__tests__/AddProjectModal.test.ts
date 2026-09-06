import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import AddProjectModal from '../AddProjectModal.svelte';
import { resetProjectsForTest } from '../../../stores/projects.svelte';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import { addComputerProject, projectAtComputerPath } from '../../../stores/computerProjects';
import { noteProject, __resetEntityIndexForTest } from '../../../transport/entityIndex';
import { takePinnedBackend } from '../../../transport/backends';

// Flush the focus-trap + effect queue so the modal is fully mounted and
// the browser has kicked off its initial BrowseDirectory.
async function flushModalBoot(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
  for (let i = 0; i < 4; i += 1) await tick();
}

function mockBrowseDirectory(
  payload: {
    path?: string;
    parent?: string;
    entries?: { name: string; isDir: boolean; hidden?: boolean; isRepo?: boolean }[];
    exists?: boolean;
  } = {},
) {
  setBindingMock('BrowseDirectory', async () => ({
    path: payload.path ?? '/Users/me',
    parent: payload.parent ?? '/Users',
    separator: '/',
    entries: (payload.entries ?? []).map((e) => ({
      name: e.name,
      isDir: e.isDir,
      hidden: e.hidden ?? false,
      isRepo: e.isRepo ?? false,
    })),
    truncated: false,
    exists: payload.exists ?? true,
  }));
}

describe('<AddProjectModal>', () => {
  beforeEach(() => {
    resetProjectsForTest();
    __resetEntityIndexForTest();
    mockBrowseDirectory();
  });

  it('renders nothing when open=false', () => {
    const { queryByRole } = render(AddProjectModal, {
      props: { open: false, onClose: () => {} },
    });
    expect(queryByRole('dialog')).toBeNull();
  });

  it('renders the modal with directory browser when open=true', async () => {
    const { getByRole, getByTestId } = render(AddProjectModal, {
      props: { open: true, onClose: () => {} },
    });
    await flushModalBoot();
    expect(getByRole('dialog')).toBeInTheDocument();
    expect(getByTestId('directory-browser-path')).toBeInTheDocument();
  });

  it('Escape closes the modal', async () => {
    const onClose = vi.fn();
    const { container } = render(AddProjectModal, {
      props: { open: true, onClose },
    });
    await flushModalBoot();
    const backdrop = container.querySelector('[data-modal-backdrop]');
    expect(backdrop).toBeTruthy();
    await fireEvent.keyDown(backdrop!, { key: 'Escape' });
    expect(onClose).toHaveBeenCalled();
  });

  it('Cancel button closes the modal', async () => {
    const onClose = vi.fn();
    const { getByTestId } = render(AddProjectModal, {
      props: { open: true, onClose },
    });
    await flushModalBoot();
    await fireEvent.click(getByTestId('add-project-cancel'));
    expect(onClose).toHaveBeenCalled();
  });

  it('Add commits the current path via CreateProject on success', async () => {
    const onClose = vi.fn();
    const onCreated = vi.fn();
    const created = {
      id: 'new-p',
      path: '/Users/me',
      name: 'me',
      sortPosition: 0,
      createdAt: 1,
      updatedAt: 1,
      archived: false,
    };
    const create = setBindingMock('CreateProject', async () => created);
    const { getByTestId } = render(AddProjectModal, {
      props: { open: true, onClose, onCreated },
    });
    await flushModalBoot();
    await fireEvent.click(getByTestId('add-project-submit'));
    for (let i = 0; i < 5; i += 1) await tick();
    expect(create).toHaveBeenCalled();
    expect(onCreated).toHaveBeenCalledWith(created);
    expect(onClose).toHaveBeenCalled();
  });

  it('shows an inline warning and calls onDuplicate when path is already a project', async () => {
    const onClose = vi.fn();
    const onDuplicate = vi.fn();
    // Seed the projects store with a matching path so the modal can map
    // "already" errors back to the existing project id.
    const existing = {
      id: 'existing-p',
      path: '/Users/me',
      name: 'me',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };
    setBindingMock('ListProjects', async () => [
      { project: existing, threadCount: 0, lastActive: 0 },
    ]);
    const { refreshProjects } = await import('../../../stores/projects.svelte');
    await refreshProjects();

    setBindingMock('CreateProject', async () => {
      throw new Error('project path already in use');
    });

    const { getByTestId } = render(AddProjectModal, {
      props: { open: true, onClose, onDuplicate },
    });
    await flushModalBoot();
    await fireEvent.click(getByTestId('add-project-submit'));
    for (let i = 0; i < 5; i += 1) await tick();
    expect(onDuplicate).toHaveBeenCalledWith('existing-p');
    expect(onClose).toHaveBeenCalled();
  });

  it('surfaces a generic backend error inline', async () => {
    setBindingMock('CreateProject', async () => {
      throw new Error('stat failed: no such file');
    });
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { getByTestId, findByTestId } = render(AddProjectModal, {
      props: { open: true, onClose: () => {} },
    });
    await flushModalBoot();
    await fireEvent.click(getByTestId('add-project-submit'));
    const err = await findByTestId('add-project-error');
    expect(err.textContent).toMatch(/stat failed/i);
    consoleSpy.mockRestore();
  });

  it('captures the selected computer for registration and distinguishes identical paths', async () => {
    const local = { id: 'local', path: '/repo', name: 'repo', sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false };
    const remote = { ...local, id: 'remote' };
    setBindingMock('ListProjects', async () => [{ project: local, threadCount: 0 }]);
    const { refreshProjects } = await import('../../../stores/projects.svelte');
    await refreshProjects();
    noteProject(local.id, '');
    setBindingMock('CreateProject', async (path) => {
      expect(takePinnedBackend()).toBe('gpu');
      expect(path).toBe('/repo');
      return remote;
    });
    await addComputerProject('gpu', '/repo');
    expect(projectAtComputerPath('', '/repo')?.id).toBe('local');
    expect(projectAtComputerPath('gpu', '/repo')?.id).toBe('remote');
  });
});
