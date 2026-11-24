// Project deletion from the sidebar (D25). Delete asks the backend what the
// deletion involves before it offers anything, and the three answers — nothing
// to say, something to say, or "I could not tell" — each get their own path.

import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ProjectContextMenu from './ProjectContextMenu.svelte';
import { tick } from 'svelte';
import { applySettingsSnapshot, resetSettingsForTest } from '../../stores/settings.svelte';
import { makeSettings } from '../../../test/helpers/settings';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import { __resetEntityIndexForTest, noteProject } from '../../transport/entityIndex';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { OBSERVE_SCOPES, pairViewOnly, pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';
import {
  setBindingMock,
  getBindingMock,
} from '../../../test/mocks/bindings-app';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import type { ProjectTarget } from './sidebarEntries';
import { createProjectActions } from './projectActions.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import type { ProjectDeletionPreview, ProjectDeletionResult } from '../../types/workflow';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';
import {
  consumePendingGroupRename,
  getThreadGroupById,
} from '../../stores/threadGroups.svelte';
import {
  getThreadFilterQuery,
  setThreadFilterQuery,
} from '../../stores/threadFilter.svelte';
import { entryRecordForTest } from '../../stores/sidebarEntryContinuity.svelte';
import { addProjectDeletedObserver } from '../../stores/projects.svelte';

// The menu takes the project frozen at the gesture that opened it, never a
// row it could read again.
function makeTarget(): ProjectTarget {
  return {
    id: 'project-1',
    name: 'Repo',
    path: '/tmp/repo',
    backend: HOME_BACKEND,
    label: 'Repo',
    threadCount: 2,
    scope: { kind: 'whole' },
  };
}

function emptyPreview(): ProjectDeletionPreview {
  return {
    projectId: 'project-1',
    runCount: 0,
    liveRunIds: [],
    worktrees: [],
    hasWork: false,
  };
}

function workPreview(): ProjectDeletionPreview {
  return {
    projectId: 'project-1',
    runCount: 3,
    liveRunIds: ['run-1'],
    worktrees: [
      {
        path: '/tmp/worktrees/run-1',
        branch: 'workflow-run-1',
        dirtyFileCount: 2,
        retained: true,
        reason: '2 uncommitted or untracked files',
      },
      {
        path: '/tmp/worktrees/run-2',
        branch: 'workflow-run-2',
        dirtyFileCount: 0,
        retained: false,
      },
    ],
    hasWork: true,
  };
}

function emptyResult(): ProjectDeletionResult {
  return { threadIds: [], retainedWorktrees: [] };
}

function renderMenu(target: ProjectTarget = makeTarget()) {
  const anchor = document.createElement('div');
  document.body.appendChild(anchor);
  // The row's runs, which its inline rename shares: a test that acts on the
  // row while a call is out holds them the way ProjectItem does.
  const actions = createProjectActions();
  const view = render(ProjectContextMenu, {
    props: {
      target,
      actions,
      anchor,
      open: true,
      onClose: () => {},
      onRename: () => {},
    },
  });
  return Object.assign(view, { actions });
}

/** A promise a test resolves or rejects when it chooses. */
function deferred<T>(): { promise: Promise<T>; reject: (err: unknown) => void } {
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((_resolve, rejectFn) => {
    reject = rejectFn;
  });
  // Nothing awaits this copy; it only keeps an unhandled rejection quiet
  // until the component's own catch runs.
  void promise.catch(() => {});
  return { promise, reject };
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
    setPageGrantsFromBootstrap(false);
    for (const toast of Array.from(getToasts())) removeToast(toast.id);
    setBindingMock('DeleteProject', async () => emptyResult());
  });

  afterEach(() => {
    setPageGrantsFromBootstrap(false);
  });

  it('omits Open in Editor in a view-only session', () => {
    setPageGrantsFromBootstrap(true);
    const open = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const { baseElement } = renderMenu();

    expect(baseElement.textContent).not.toContain('Open in Editor');
    expect(open).not.toHaveBeenCalled();
  });

  it('takes the plain confirm when the project owns no workflow work', async () => {
    setBindingMock('ProjectDeletionPreview', async () => emptyPreview());
    const { baseElement } = renderMenu();

    await selectDelete(baseElement);
    await waitFor(() => {
      expect(baseElement.textContent).toContain('Permanently delete "Repo"');
    });
    // The richer dialog is for projects that own work; this one must not appear.
    expect(baseElement.querySelector('[data-testid="project-delete-dialog"]')).toBeNull();

    const confirm = Array.from(baseElement.querySelectorAll('button')).find(
      (el) => el.textContent?.trim() === 'Delete',
    );
    await fireEvent.click(confirm as Element);

    await waitFor(() => {
      expect(getBindingMock('DeleteProject')).toHaveBeenCalledWith('project-1');
    });
  });

  it('tells what keeps state by entry key that a confirmed delete deleted the project, and an archive did not', async () => {
    const deleted: string[] = [];
    const stop = addProjectDeletedObserver((key) => deleted.push(key));
    try {
      setBindingMock('ArchiveProject', async () => undefined);
      const archive = renderMenu();
      const archiveItem = Array.from(archive.baseElement.querySelectorAll('[role="menuitem"]'))
        .find((el) => el.textContent?.trim() === 'Archive Project');
      await fireEvent.click(archiveItem as Element);
      await waitFor(() => expect(archive.baseElement.querySelector('[role="dialog"]')).not.toBeNull());
      await fireEvent.click(Array.from(archive.baseElement.querySelectorAll('button'))
        .find((el) => el.textContent?.trim() === 'Archive') as Element);
      await waitFor(() => expect(toastMessages()).toContain('Archived project "Repo".'));
      expect(deleted).toEqual([]);
      archive.unmount();

      setBindingMock('ProjectDeletionPreview', async () => emptyPreview());
      const { baseElement } = renderMenu();
      await selectDelete(baseElement);
      await waitFor(() => expect(baseElement.textContent).toContain('Permanently delete "Repo"'));
      await fireEvent.click(Array.from(baseElement.querySelectorAll('button'))
        .find((el) => el.textContent?.trim() === 'Delete') as Element);
      await waitFor(() => expect(deleted).toEqual(['project-1']));
    } finally {
      stop();
    }
  });

  // What a confirmation promises is what the one call affects.
  describe('what the confirmations say the call affects', () => {
    const oneOfSeveral = (): ProjectTarget => ({
      ...makeTarget(),
      label: 'app',
      threadCount: 3,
      scope: { kind: 'oneOfSeveral', computerName: 'Laptop' },
    });
    const normalized = (el: Element | null) => (el?.textContent ?? '').replace(/\s+/g, ' ').trim();

    async function openDelete(target: ProjectTarget, preview: ProjectDeletionPreview): Promise<HTMLElement> {
      setBindingMock('ProjectDeletionPreview', async () => preview);
      const { baseElement } = renderMenu(target);
      await selectDelete(baseElement);
      await waitFor(() => expect(baseElement.querySelector('[role="dialog"]')).not.toBeNull());
      return baseElement;
    }

    async function openArchive(target: ProjectTarget): Promise<HTMLElement> {
      const { baseElement } = renderMenu(target);
      const item = Array.from(baseElement.querySelectorAll('[role="menuitem"]'))
        .find((el) => el.textContent?.trim() === 'Archive Project');
      await fireEvent.click(item as Element);
      await waitFor(() => expect(baseElement.querySelector('[role="dialog"]')).not.toBeNull());
      return baseElement;
    }

    it.each([
      ['names the one checkout a delete removes, and its own threads, when the entry has others', oneOfSeveral,
        'Permanently delete "app" on Laptop and all 3 threads it contains there. Checkouts on other computers are not affected. This cannot be undone.'],
      ["keeps today's delete text for an entry with one checkout", makeTarget,
        'Permanently delete "Repo" and all 2 threads it contains. This cannot be undone.'],
    ])('%s', async (_name, target, sentence) => {
      const page = await openDelete(target(), emptyPreview());
      expect(normalized(page.querySelector('[role="dialog"]'))).toContain(sentence);
    });

    it('says the same in the dialog for a project with workflow work', async () => {
      const page = await openDelete(oneOfSeveral(), workPreview());
      expect(normalized(page.querySelector('[data-testid="project-delete-dialog"]'))).toContain(
        'Permanently delete "app" on Laptop and all 3 threads it contains there. Checkouts on other computers are not affected.',
      );
    });

    it.each([
      ['names the one checkout an archive hides when the entry has others', oneOfSeveral,
        'Archive "app" on Laptop. Its threads there remain and it can be unarchived from Settings. Checkouts on other computers are not affected.'],
      ["keeps today's archive text for an entry with one checkout", makeTarget,
        'Hide "Repo" from the sidebar. Threads remain and the project can be unarchived from Settings.'],
    ])('%s', async (_name, target, sentence) => {
      const page = await openArchive(target());
      expect(normalized(page.querySelector('[role="dialog"]'))).toContain(sentence);
    });
  });

  it('says what the cleanup does, that branches are kept, and which checkouts stay', async () => {
    setBindingMock('ProjectDeletionPreview', async () => workPreview());
    const { baseElement } = renderMenu();

    await selectDelete(baseElement);
    await waitFor(() => {
      expect(baseElement.querySelector('[data-testid="project-delete-dialog"]')).not.toBeNull();
    });
    const work = baseElement.querySelector('[data-testid="project-delete-work"]');
    expect(work?.textContent).toContain('3 workflow runs will be deleted with it');
    expect(work?.textContent).toContain('1 still working');
    expect(
      baseElement.querySelector('[data-testid="project-delete-branches"]')?.textContent,
    ).toContain('branches are kept');

    // Only the checkout that will survive is listed — the clean one costs the
    // user nothing and needs no row.
    const rows = baseElement.querySelectorAll('[data-testid="project-delete-worktree"]');
    expect(rows).toHaveLength(1);
    expect(rows[0].textContent).toContain('/tmp/worktrees/run-1');
    expect(rows[0].textContent).toContain('workflow-run-1');
    expect(rows[0].textContent).toContain('2 uncommitted or untracked files');
    expect(baseElement.textContent).not.toContain('/tmp/worktrees/run-2');

    const confirm = baseElement.querySelector('[data-testid="project-delete-confirm"]');
    expect(confirm?.textContent?.trim()).toBe('Delete Project');
    await fireEvent.click(confirm as Element);

    await waitFor(() => {
      expect(getBindingMock('DeleteProject')).toHaveBeenCalledWith('project-1');
    });
  });

  it('warns about the checkouts the deletion could not remove', async () => {
    setBindingMock('ProjectDeletionPreview', async () => emptyPreview());
    setBindingMock('DeleteProject', async () => ({
      threadIds: ['thread-1'],
      retainedWorktrees: [
        {
          path: '/tmp/worktrees/run-1',
          branch: 'workflow-run-1',
          reason: '2 uncommitted or untracked files',
        },
      ],
    }));
    const { baseElement } = renderMenu();

    await selectDelete(baseElement);
    await waitFor(() => {
      expect(baseElement.textContent).toContain('Permanently delete "Repo"');
    });
    const confirm = Array.from(baseElement.querySelectorAll('button')).find(
      (el) => el.textContent?.trim() === 'Delete',
    );
    await fireEvent.click(confirm as Element);

    await waitFor(() => {
      expect(toastMessages().join(' ')).toContain('Deleted project "Repo"');
    });
    const warning = getToasts().find((toast) => toast.type === 'warning');
    expect(warning?.message).toContain('1 checkout left in place');
    expect(warning?.message).toContain('/tmp/worktrees/run-1');
    expect(warning?.message).toContain('2 uncommitted or untracked files');
  });

  // A delete that fails leaves the dialog the user is looking at in place,
  // so the retry is one click. The run that failed is spent, so that retry
  // is a run of its own carrying the same project and the same answer.
  it('keeps the preview dialog up when the delete fails, and retries from it', async () => {
    setBindingMock('ProjectDeletionPreview', async () => workPreview());
    const del = setBindingMock('DeleteProject', vi.fn(async () => {
      throw new Error('project is busy');
    }));
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { baseElement } = renderMenu();

    await selectDelete(baseElement);
    await waitFor(() => {
      expect(baseElement.querySelector('[data-testid="project-delete-dialog"]')).not.toBeNull();
    });
    const confirmButton = (): Element => {
      const button = baseElement.querySelector('[data-testid="project-delete-confirm"]');
      if (button === null) throw new Error('the dialog has no confirm button');
      return button;
    };

    await fireEvent.click(confirmButton());
    await waitFor(() => {
      expect(toastMessages().join(' ')).toContain('Project is busy');
    });
    // Still there, with what the computer answered about the project, and
    // no longer saying a deletion is under way.
    const dialog = baseElement.querySelector('[data-testid="project-delete-dialog"]');
    expect(dialog).not.toBeNull();
    expect(baseElement.querySelector('[data-testid="project-delete-work"]')?.textContent)
      .toContain('3 workflow runs');

    await fireEvent.click(confirmButton());
    await waitFor(() => expect(del).toHaveBeenCalledTimes(2));
    expect(del).toHaveBeenNthCalledWith(2, 'project-1');
    consoleError.mockRestore();
  });

  // A delete that fails after the user has moved on must leave the row as
  // they left it: its catch answers for a project they are no longer
  // looking at, and a dialog raised from it would be that project's.
  it('brings nothing back when the delete fails after the dialog was cancelled', async () => {
    setBindingMock('ProjectDeletionPreview', async () => workPreview());
    const call = deferred<ProjectDeletionResult>();
    setBindingMock('DeleteProject', () => call.promise);
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { baseElement, actions } = renderMenu();

    await selectDelete(baseElement);
    await waitFor(() => {
      expect(baseElement.querySelector('[data-testid="project-delete-dialog"]')).not.toBeNull();
    });
    await fireEvent.click(baseElement.querySelector('[data-testid="project-delete-confirm"]')!);

    // The user gives up on it while the call is out.
    const cancel = Array.from(baseElement.querySelectorAll('button')).find(
      (button) => button.textContent?.trim() === 'Cancel',
    );
    await fireEvent.click(cancel!);
    await tick();
    expect(baseElement.querySelector('[data-testid="project-delete-dialog"]')).toBeNull();

    call.reject(new Error('project is busy'));
    await waitFor(() => {
      expect(toastMessages().join(' ')).toContain('Project is busy');
    });
    expect(baseElement.querySelector('[data-testid="project-delete-dialog"]')).toBeNull();
    expect(actions.current).toBeNull();
    consoleError.mockRestore();
  });

  it('leaves the newer action alone when the delete fails behind it', async () => {
    setBindingMock('ProjectDeletionPreview', async () => workPreview());
    const call = deferred<ProjectDeletionResult>();
    setBindingMock('DeleteProject', () => call.promise);
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { baseElement, actions } = renderMenu();

    await selectDelete(baseElement);
    await waitFor(() => {
      expect(baseElement.querySelector('[data-testid="project-delete-dialog"]')).not.toBeNull();
    });
    await fireEvent.click(baseElement.querySelector('[data-testid="project-delete-confirm"]')!);

    // A newer gesture on the row, which owns it from here on.
    const archive = actions.start('archive', { ...makeTarget(), id: 'project-2', label: 'Other' });
    archive.confirm();
    await tick();
    expect(baseElement.textContent).toContain('Hide "Other"');

    call.reject(new Error('project is busy'));
    await waitFor(() => {
      expect(toastMessages().join(' ')).toContain('Project is busy');
    });
    expect(baseElement.querySelector('[data-testid="project-delete-dialog"]')).toBeNull();
    expect(baseElement.textContent).toContain('Hide "Other"');
    expect(actions.current).toBe(archive);
    consoleError.mockRestore();
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
    consoleError.mockRestore();
  });
});

describe('<ProjectContextMenu> New Group…', () => {
  beforeEach(() => {
    setPageGrantsFromBootstrap(false);
    setThreadFilterQuery('');
    for (const toast of Array.from(getToasts())) removeToast(toast.id);
  });

  function newGroupItem(baseElement: HTMLElement): Element {
    const item = Array.from(baseElement.querySelectorAll('[role="menuitem"]')).find(
      (el) => el.textContent?.trim() === 'New Group…',
    );
    if (!item) throw new Error('New Group… not rendered');
    return item;
  }

  it('creates the group in this project, expands it, and asks for the rename', async () => {
    const create = setBindingMock('CreateThreadGroup', vi.fn(
      async (projectId: string, name: string) => ({
        id: 'g-new',
        projectId,
        name,
        createdAt: 0,
        updatedAt: 0,
      }),
    ));
    const { baseElement } = renderMenu();

    const item = Array.from(baseElement.querySelectorAll('[role="menuitem"]')).find(
      (el) => el.textContent?.trim() === 'New Group…',
    );
    await fireEvent.click(item as Element);
    for (let i = 0; i < 5; i += 1) await Promise.resolve();

    expect(create).toHaveBeenCalledWith('project-1', 'New Group');
    expect(getThreadGroupById('g-new')).toBeTruthy();
    expect(entryRecordForTest('project-1').expanded).toBe(true);
    // The row that has not mounted yet owes the inline rename.
    expect(consumePendingGroupRename('g-new')).toBe(true);
  });

  it('clears the sidebar search first — the filter would hide the new row', async () => {
    // A brand-new group is empty and named "New Group", so an active query
    // drops it from the project's bucket: it would be created, never render,
    // and never open the rename the user is about to type into.
    setBindingMock('CreateThreadGroup', vi.fn(async (projectId: string, name: string) => ({
      id: 'g-new',
      projectId,
      name,
      createdAt: 0,
      updatedAt: 0,
    })));
    setThreadFilterQuery('spike');
    const { baseElement } = renderMenu();

    await fireEvent.click(newGroupItem(baseElement));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();

    expect(getThreadFilterQuery()).toBe('');
  });
});

// The create items exist for the phone only: on the desktop they are the
// header's hover controls, and the desktop menu stays what it was.
describe('<ProjectContextMenu> compact create items', () => {
  afterEach(() => {
    setCompactLayoutForTest(false);
    resetToLocalPage();
  });

  function renderMenu(extra: Record<string, unknown> = {}) {
    return render(ProjectContextMenu, {
      props: {
        target: makeTarget(),
        actions: createProjectActions(),
        anchor: document.body,
        open: true,
        onClose: vi.fn(),
        onRename: vi.fn(),
        ...extra,
      } as never,
    });
  }

  it('are absent on the desktop', () => {
    const { queryByRole } = renderMenu();
    expect(queryByRole('menuitem', { name: 'New Thread' })).toBeNull();
    expect(queryByRole('menuitem', { name: 'New Terminal' })).toBeNull();
  });

  it('run the row handlers and close the menu under compact', async () => {
    setCompactLayoutForTest(true);
    const onClose = vi.fn();
    const onNewThread = vi.fn();
    const onNewTerminal = vi.fn();
    const { getByRole } = renderMenu({ onClose, onNewThread, onNewTerminal });
    await fireEvent.click(getByRole('menuitem', { name: 'New Thread' }));
    expect(onNewThread).toHaveBeenCalledTimes(1);
    await fireEvent.click(getByRole('menuitem', { name: 'New Terminal' }));
    expect(onNewTerminal).toHaveBeenCalledTimes(1);
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  it('go inert, saying why, for a session without the grant', async () => {
    setCompactLayoutForTest(true);
    await pairViewOnly();
    const { getByRole } = renderMenu();
    const item = getByRole('menuitem', { name: 'New Terminal' });
    expect(item.getAttribute('aria-disabled')).toBe('true');
    expect(item.getAttribute('title')).toBe('Not granted to this device');
  });
});

// New Assistant… is an assistant's entry point: gone, on the desktop and
// under compact, while assistants are turned off.
describe('<ProjectContextMenu> New Assistant…', () => {
  afterEach(() => setCompactLayoutForTest(false));

  for (const compact of [false, true]) {
    it(`is offered while assistants are on and absent while they are off${compact ? ' under compact' : ''}`, async () => {
      setCompactLayoutForTest(compact);
      const view = renderMenu();
      expect(view.queryByRole('menuitem', { name: 'New Assistant…' })).not.toBeNull();
      applySettingsSnapshot(makeSettings({ assistantsEnabled: false }));
      await tick();
      expect(view.queryByRole('menuitem', { name: 'New Assistant…' })).toBeNull();
    });
  }

  // It creates on the project's computer: a project on the laptop offers it
  // while the laptop has assistants on, whatever this computer has.
  it("follows the project's computer", async () => {
    stageBackend();
    noteProject('project-1', 'laptop');
    try {
      applySettingsSnapshot(makeSettings({ assistantsEnabled: false }));
      // The target carries the computer, captured when the row opened the menu.
      const view = renderMenu({ ...makeTarget(), backend: 'laptop' });
      expect(view.queryByRole('menuitem', { name: 'New Assistant…' })).not.toBeNull();
      applySettingsSnapshot(makeSettings({ assistantsEnabled: true }));
      applySettingsSnapshot(makeSettings({ assistantsEnabled: false }), 'laptop');
      await tick();
      expect(view.queryByRole('menuitem', { name: 'New Assistant…' })).toBeNull();
    } finally {
      resetStagedBackends();
      __resetEntityIndexForTest();
      resetSettingsForTest();
    }
  });
});

describe('<ProjectContextMenu> project writes without the grant', () => {
  // Rename, New Group… and Archive write under `threads:operate`. Delete
  // also previews the cleanup through ProjectDeletionPreview (`git:operate`),
  // so it stays inert until both are held.
  const INERT = 'Not granted to this device';

  afterEach(() => {
    resetToLocalPage();
  });

  function item(baseElement: HTMLElement, label: string): HTMLElement {
    const node = Array.from(baseElement.querySelectorAll('[role="menuitem"]'))
      .find((el) => el.textContent?.trim() === label);
    if (!node) throw new Error(`${label} not rendered`);
    return node as HTMLElement;
  }

  it('renders every write inert, saying why, for a view-only session', async () => {
    await pairViewOnly();
    const preview = setBindingMock('ProjectDeletionPreview', vi.fn(async () => emptyPreview()));
    const { baseElement } = renderMenu();
    for (const label of ['Rename Project', 'New Group…', 'Archive Project', 'Delete Project']) {
      const row = item(baseElement, label);
      expect(row.getAttribute('aria-disabled'), label).toBe('true');
      expect(row.getAttribute('title'), label).toBe(INERT);
    }
    await fireEvent.click(item(baseElement, 'Delete Project'));
    expect(preview).not.toHaveBeenCalled();
  });

  it('keeps Delete inert with threads:operate but no git:operate', async () => {
    await pairWithScopes([...OBSERVE_SCOPES, 'threads:operate']);
    const { baseElement } = renderMenu();
    expect(item(baseElement, 'Rename Project').getAttribute('aria-disabled')).not.toBe('true');
    expect(item(baseElement, 'Archive Project').getAttribute('aria-disabled')).not.toBe('true');
    const del = item(baseElement, 'Delete Project');
    expect(del.getAttribute('aria-disabled')).toBe('true');
    expect(del.getAttribute('title')).toBe(INERT);
  });
});
