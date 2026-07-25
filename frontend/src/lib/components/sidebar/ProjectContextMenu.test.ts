// Project deletion consent (D25). Delete asks the backend what it would
// destroy before it offers anything, and the three answers — nothing,
// something, or "I could not tell" — each get their own path.

import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ProjectContextMenu from './ProjectContextMenu.svelte';
import {
  resetBindingMocks,
  setBindingMock,
  getBindingMock,
} from '../../../test/mocks/bindings-app';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import type { ProjectWithCounts } from '../../types/models';
import type { ProjectDeletionPreview } from '../../types/workflow';

function makeProject(): ProjectWithCounts {
  return {
    project: {
      id: 'project-1',
      path: '/tmp/repo',
      name: 'Repo',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    },
    threadCount: 2,
  };
}

function emptyPreview(): ProjectDeletionPreview {
  return {
    projectId: 'project-1',
    rootRunIds: [],
    runCount: 0,
    liveRunIds: [],
    automationCount: 0,
    worktrees: [],
    hasWork: false,
  };
}

function lossyPreview(): ProjectDeletionPreview {
  return {
    projectId: 'project-1',
    rootRunIds: ['run-1'],
    runCount: 3,
    liveRunIds: ['run-1'],
    automationCount: 1,
    worktrees: [
      {
        itemId: 'run-1',
        path: '/tmp/worktrees/run-1',
        branch: 'workflow-run-1',
        base: 'main',
        present: true,
        registered: true,
        dirtyFiles: ['a.txt'],
        dirtyFileCount: 1,
        unmergedCommits: [],
        unmergedCommitCount: 2,
      },
    ],
    hasWork: true,
  };
}

function renderMenu() {
  const anchor = document.createElement('div');
  document.body.appendChild(anchor);
  return render(ProjectContextMenu, {
    props: {
      project: makeProject(),
      anchor,
      open: true,
      onClose: () => {},
      onRename: () => {},
    },
  });
}

async function selectDelete(baseElement: HTMLElement): Promise<void> {
  const item = Array.from(baseElement.querySelectorAll('[role="menuitem"]')).find(
    (el) => el.textContent?.trim() === 'Delete Project',
  );
  expect(item).toBeTruthy();
  await fireEvent.click(item as Element);
}

function toastMessages(): string[] {
  return getToasts().map((toast) => toast.message);
}

describe('<ProjectContextMenu> delete flow', () => {
  beforeEach(() => {
    resetBindingMocks();
    for (const toast of [...getToasts()]) removeToast(toast.id);
    setBindingMock('DeleteProject', async () => []);
    setBindingMock('DeleteProjectDiscardingWorkflowWork', async () => []);
  });

  it('takes the plain confirm and deletes without consent when nothing would be destroyed', async () => {
    setBindingMock('ProjectDeletionPreview', async () => emptyPreview());
    const { baseElement } = renderMenu();

    await selectDelete(baseElement);
    await waitFor(() => {
      expect(baseElement.textContent).toContain('Permanently delete "Repo"');
    });
    // The loss dialog is for projects that own work; this one must not appear.
    expect(baseElement.querySelector('[data-testid="project-delete-dialog"]')).toBeNull();

    const confirm = Array.from(baseElement.querySelectorAll('button')).find(
      (el) => el.textContent?.trim() === 'Delete',
    );
    await fireEvent.click(confirm as Element);

    await waitFor(() => {
      expect(getBindingMock('DeleteProject')).toHaveBeenCalledWith('project-1');
    });
    // The destructive form is a different method, so it is refused from a
    // non-loopback client; a project with nothing to destroy never reaches it.
    expect(getBindingMock('DeleteProjectDiscardingWorkflowWork')).not.toHaveBeenCalled();
    await waitFor(() => {
      expect(getBindingMock('DeleteProject')).toHaveBeenCalledTimes(1);
    });
  });

  it('shows the loss and deletes with consent when the project owns workflow work', async () => {
    setBindingMock('ProjectDeletionPreview', async () => lossyPreview());
    const { baseElement } = renderMenu();

    await selectDelete(baseElement);
    await waitFor(() => {
      expect(baseElement.querySelector('[data-testid="project-delete-dialog"]')).not.toBeNull();
    });
    const work = baseElement.querySelector('[data-testid="project-delete-work"]');
    expect(work?.textContent).toContain('3 workflow runs and 1 automation will be deleted');
    expect(work?.textContent).toContain('1 still working');
    const rows = baseElement.querySelectorAll('[data-testid="project-delete-worktree"]');
    expect(rows).toHaveLength(1);
    expect(rows[0].textContent).toContain('/tmp/worktrees/run-1');
    expect(rows[0].textContent).toContain('workflow-run-1');
    expect(rows[0].textContent).toContain('1 dirty file');
    expect(rows[0].textContent).toContain('2 unmerged commits');

    const confirm = baseElement.querySelector('[data-testid="project-delete-confirm"]');
    await fireEvent.click(confirm as Element);

    await waitFor(() => {
      expect(getBindingMock('DeleteProjectDiscardingWorkflowWork')).toHaveBeenCalledWith(
        'project-1',
      );
    });
    // Consent is the method, not an argument: the plain form must stay unused.
    expect(getBindingMock('DeleteProject')).not.toHaveBeenCalled();
  });

  it('stops at the preview failure instead of deleting anything', async () => {
    setBindingMock('ProjectDeletionPreview', async () => {
      throw new Error('git is unavailable');
    });
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { baseElement } = renderMenu();

    await selectDelete(baseElement);
    await waitFor(() => {
      expect(toastMessages().join(' ')).toContain('Git is unavailable');
    });
    expect(baseElement.querySelector('[data-testid="project-delete-dialog"]')).toBeNull();
    expect(baseElement.textContent).not.toContain('Permanently delete "Repo"');
    expect(getBindingMock('DeleteProject')).not.toHaveBeenCalled();
    expect(getBindingMock('DeleteProjectDiscardingWorkflowWork')).not.toHaveBeenCalled();
    consoleError.mockRestore();
  });
});
