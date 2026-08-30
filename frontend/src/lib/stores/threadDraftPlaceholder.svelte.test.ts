// stores/threadDraftPlaceholder.svelte.test.ts
//
// threadDraftPlaceholder.svelte.ts through the pane: staging "+ New" over
// an unsent draft, the placeholder terminals and worktree intent a restage
// or a cwd change must clean up, and materialization into a real thread.

import { beforeEach, describe, expect, it } from 'vitest';
import { createThreadPane } from './thread.svelte';
import {
  resetForTest as resetWorktreeIntent,
  setAttachBranch,
  setThreadEnvMode,
  worktreeIntentForThread,
} from './worktreeIntent.svelte';
import { type Project } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { buildPane, makeThread } from '../../test/helpers/chat';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import {
  getExistingThreadTerminalState,
  getThreadTerminalState,
} from '../components/terminal/terminalStore.svelte';

describe('threadDraftPlaceholder', () => {
  beforeEach(installThreadPaneTestEnv);

  it('drops stale placeholder worktree intent when "+ New" replaces an unsent draft', () => {
    // Repeated "+ New" without typing would otherwise leak worktree
    // entries keyed by the prior placeholder id — they're unreachable
    // (no Thread points at them) but stay in the store until reset.
    // Verify startDraftPlaceholder cleans them up before staging the
    // next placeholder.
    resetWorktreeIntent();
    try {
      const pane = createThreadPane();
      const projectA: Project = {
        id: 'p-1',
        path: '/tmp/p1',
        name: 'p1',
        sortPosition: 0,
        createdAt: 0,
        updatedAt: 0,
        archived: false,
      };
      // Use a distinct project for the second placeholder so the
      // synthesised draft id differs even when both startDraftPlaceholder
      // calls land in the same millisecond — otherwise the cleanup and
      // a "no-op" cannot be distinguished by querying the same id back.
      const projectB: Project = {
        ...projectA,
        id: 'p-2',
        path: '/tmp/p2',
        name: 'p2',
      };

      pane.startDraftPlaceholder(projectA, 'chat');
      const firstPlaceholder = pane.thread;
      expect(firstPlaceholder).not.toBeNull();
      expect(firstPlaceholder!.id.startsWith('draft:')).toBe(true);

      setThreadEnvMode(firstPlaceholder!, 'new-worktree');
      setAttachBranch(firstPlaceholder!, 'feature/x');

      expect(worktreeIntentForThread(firstPlaceholder!).attachBranch).toBe(
        'feature/x',
      );

      pane.startDraftPlaceholder(projectB, 'chat');
      expect(pane.thread?.id).not.toBe(firstPlaceholder!.id);

      // The intent stores key by thread.id — query against the prior
      // placeholder thread to confirm the entries are gone.
      expect(worktreeIntentForThread(firstPlaceholder!).mode).toBe('local');
      expect(worktreeIntentForThread(firstPlaceholder!).attachBranch).toBe('');
    } finally {
      resetWorktreeIntent();
    }
  });

  it('closes placeholder terminals when "+ New" replaces an unsent draft', () => {
    const pane = createThreadPane();
    const projectA: Project = {
      id: 'p-1',
      path: '/tmp/p1',
      name: 'p1',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };
    const projectB: Project = {
      ...projectA,
      id: 'p-2',
      path: '/tmp/p2',
      name: 'p2',
    };

    pane.startDraftPlaceholder(projectA, 'chat');
    const firstPlaceholderId = pane.thread!.id;
    getThreadTerminalState(firstPlaceholderId).addTab({
      terminalID: 'term-1',
      threadID: firstPlaceholderId,
      shell: '/bin/sh',
      cwd: '/tmp/p1',
      rows: 24,
      cols: 80,
      pid: 123,
      startedAt: 1,
      running: true,
      exitCode: 0,
      exitReason: '',
    });
    pane.setShowTerminal(true);
    const close = setBindingMock('CloseThreadTerminals', async () => undefined);

    pane.startDraftPlaceholder(projectB, 'chat');

    expect(close.mock.calls[0]).toEqual([firstPlaceholderId]);
    expect(getExistingThreadTerminalState(firstPlaceholderId)).toBeNull();
    expect(pane.showTerminal).toBe(false);
    expect(pane.thread?.projectId).toBe('p-2');
  });

  it('closes placeholder terminals when the placeholder workspace cwd changes', () => {
    const pane = createThreadPane();
    const project: Project = {
      id: 'p-1',
      path: '/tmp/project',
      name: 'project',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };

    pane.startDraftPlaceholder(project, 'chat');
    const placeholderId = pane.thread!.id;
    getThreadTerminalState(placeholderId).addTab({
      terminalID: 'term-1',
      threadID: placeholderId,
      shell: '/bin/sh',
      cwd: '/tmp/project',
      rows: 24,
      cols: 80,
      pid: 123,
      startedAt: 1,
      running: true,
      exitCode: 0,
      exitReason: '',
    });
    pane.setShowTerminal(true);
    const close = setBindingMock('CloseThreadTerminals', async () => undefined);

    pane.applyDraftPlaceholderWorkspace({
      workspacePath: '/tmp/project-worktree',
      worktreePath: '/tmp/project-worktree',
      branch: 'feature/x',
    });

    expect(close.mock.calls[0]).toEqual([placeholderId]);
    expect(getExistingThreadTerminalState(placeholderId)).toBeNull();
    expect(pane.showTerminal).toBe(false);
    expect(pane.thread?.workspacePath).toBe('/tmp/project-worktree');
  });

  it('rejects late terminal opens after a placeholder cwd change starts cleanup', () => {
    const pane = createThreadPane();
    const project: Project = {
      id: 'p-1',
      path: '/tmp/project',
      name: 'project',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };

    pane.startDraftPlaceholder(project, 'chat');
    const placeholderId = pane.thread!.id;
    pane.setShowTerminal(true);
    const close = setBindingMock('CloseThreadTerminals', async () => undefined);

    expect(pane.canAdoptOpenedTerminal(placeholderId, '/tmp/project')).toBe(
      true,
    );
    pane.applyDraftPlaceholderWorkspace({
      workspacePath: '/tmp/project-worktree',
      worktreePath: '/tmp/project-worktree',
      branch: 'feature/x',
    });

    expect(close.mock.calls[0]).toEqual([placeholderId]);
    expect(pane.canAdoptOpenedTerminal(placeholderId, '/tmp/project')).toBe(
      false,
    );
  });

  it('migrates placeholder terminals when content materializes the thread', async () => {
    const pane = createThreadPane();
    const project: Project = {
      id: 'p-1',
      path: '/tmp/project',
      name: 'project',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };

    pane.startDraftPlaceholder(project, 'chat', {
      provider: 'claude',
      model: 'm',
      workspacePath: '/tmp/project',
      branch: 'main',
    });
    const placeholderId = pane.thread!.id;
    getThreadTerminalState(placeholderId).addTab({
      terminalID: 'term-1',
      threadID: placeholderId,
      shell: '/bin/sh',
      cwd: '/tmp/project',
      rows: 24,
      cols: 80,
      pid: 123,
      startedAt: 1,
      running: true,
      exitCode: 0,
      exitReason: '',
    });
    pane.setShowTerminal(true);
    setBindingMock('CreateThread', async () =>
      makeThread({
        id: 'thread-real',
        projectId: 'p-1',
        projectPath: '/tmp/project',
        workspacePath: '/tmp/project',
        branch: 'main',
        isDraft: true,
      }),
    );
    const move = setBindingMock('MoveThreadTerminals', async () => [
      {
        terminalID: 'term-1',
        threadID: 'thread-real',
        shell: '/bin/sh',
        cwd: '/tmp/project',
        rows: 24,
        cols: 80,
        pid: 123,
        startedAt: 1,
        running: true,
        exitCode: 0,
        exitReason: '',
      },
    ]);

    const threadId = await pane.ensureMaterializedThread();

    expect(threadId).toBe('thread-real');
    expect(move.mock.calls[0]).toEqual([placeholderId, 'thread-real']);
    expect(getExistingThreadTerminalState(placeholderId)).toBeNull();
    const migrated = getExistingThreadTerminalState('thread-real');
    expect(migrated?.tabs).toHaveLength(1);
    expect(migrated?.tabs[0]?.summary.threadID).toBe('thread-real');
    expect(pane.threadId).toBe('thread-real');
    expect(pane.showTerminal).toBe(true);
  });

  it('keeps selected workspace fields when applying placeholder model defaults', () => {
    const pane = createThreadPane();
    const project: Project = {
      id: 'p-1',
      path: '/tmp/project',
      name: 'project',
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    };

    pane.startDraftPlaceholder(project, 'chat');
    pane.applyDraftPlaceholderWorkspace({
      workspacePath: '/tmp/project-worktree',
      worktreePath: '/tmp/project-worktree',
      branch: 'feature/x',
    });
    pane.applyDraftPlaceholderDefaults({
      provider: 'codex',
      model: 'gpt-5.4',
      reasoningEffort: 'high',
      fastMode: true,
      contextWindow: 200000,
      runtimeMode: 'full-access',
      workspacePath: '/tmp/other',
      branch: 'main',
    });

    expect(pane.thread?.provider).toBe('codex');
    expect(pane.thread?.model).toBe('gpt-5.4');
    expect(pane.thread?.workspacePath).toBe('/tmp/project-worktree');
    expect(pane.thread?.worktreePath).toBe('/tmp/project-worktree');
    expect(pane.thread?.branch).toBe('feature/x');
  });

  it('migrates worktree intent when an empty materialized draft returns to a placeholder', async () => {
    resetWorktreeIntent();
    try {
      const pane = await buildPane(
        makeThread({
          id: 'materialized-draft',
          projectId: 'p-1',
          projectPath: '/tmp/project',
          workspacePath: '/tmp/project',
          mode: 'chat',
          isDraft: true,
        }),
      );

      setThreadEnvMode(pane.thread!, 'new-worktree');
      setAttachBranch(pane.thread!, 'feature/x');
      expect(worktreeIntentForThread(pane.thread!).attachBranch).toBe(
        'feature/x',
      );

      const oldThread = pane.thread!;
      expect(pane.dematerializeEmptyDraftThread()).toBe(true);
      expect(pane.thread?.id).not.toBe(oldThread.id);
      expect(pane.thread?.id.startsWith('draft:')).toBe(true);
      expect(worktreeIntentForThread(oldThread).mode).toBe('local');
      expect(worktreeIntentForThread(pane.thread!).attachBranch).toBe(
        'feature/x',
      );
    } finally {
      resetWorktreeIntent();
    }
  });
});
