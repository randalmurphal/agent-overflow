import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ChatHeaderActions from './ChatHeaderActions.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { resetEditorsForTest } from '../../stores/editors.svelte';
import type { GitStatus } from '../../types/git';
import type { Thread } from '../../types/models';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { buildPane as buildRegisteredPane, makeThread as makeBaseThread } from '../../../test/helpers/chat';

vi.mock('../../stores/threadCreation.svelte', () => ({ openTerminalThread: vi.fn() }));

// Pin transport connected so the attach $effect actually subscribes (the real
// store reads from wsClient, never initialised in test scope).
vi.mock('../../stores/transportStatus.svelte', () => ({
  getTransportStatus: () => ({ status: 'connected', nextAttemptAt: null }),
  retryTransport: () => {},
  resetTransportStatusForTest: () => {},
}));

// Svelte transitions (Popover/Menu inside GitActionsControl) poke
// Element.animate on mount; jsdom lacks it.
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

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return makeBaseThread({
    title: 'Example',
    workspacePath: '/workspace',
    projectPath: '/workspace',
    branch: 'main',
    ...overrides,
  });
}

function status(overrides: Partial<GitStatus> = {}): GitStatus {
  return {
    isRepo: true,
    branch: 'main',
    isDefaultBranch: true,
    hasChanges: false,
    insertions: 0,
    deletions: 0,
    fileCount: 0,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    forge: 'github',
    ...overrides,
  };
}

async function buildPane(thread = makeThread()) {
  return buildRegisteredPane(thread);
}

async function flush(n = 8): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

function installSubscribeMock(initial: GitStatus, id = 'sub-1') {
  const subscribeFn = setBindingMock('GitStatusSubscribe', async () => ({ id, status: initial }));
  setBindingMock('GitStatusUnsubscribe', async () => {});
  return { id, subscribeFn };
}

describe('<ChatHeaderActions> badge gating', () => {
  beforeEach(async () => {
    resetPanesForTest();
    resetEditorsForTest();
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    setBindingMock('UpdateThreadBranch', async () => makeThread());
    // The Open-in-editor control loads this catalog on mount.
    setBindingMock('ListAvailableEditors', async () => []);
    setBindingMock('GetEditorSettings', async () => ({ preference: '' }));
    await loadSettings();
  });

  it('shows the PR badge and workspace +/- on a chat thread with an open PR', async () => {
    const pane = await buildPane();
    installSubscribeMock(
      status({
        insertions: 4,
        deletions: 1,
        forge: 'github',
        openPrUrl: 'https://github.com/o/r/pull/7',
        openPrNumber: 7,
      }),
    );
    const { getByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();

    expect(getByTestId('chat-header-pr-badge').textContent?.replace(/\s+/g, '')).toBe('PR#7');
    expect(getByTestId('review-toggle')).toBeTruthy();
    expect(getByTestId('workspace-diff-counts').textContent).toContain('+4');
    expect(getByTestId('workspace-diff-counts').textContent).toContain('-1');
  });

  it('hides the PR badge but keeps the workspace +/- when there is no open PR', async () => {
    const pane = await buildPane();
    installSubscribeMock(status({ openPrUrl: '' }));
    const { queryByTestId, getByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();

    expect(queryByTestId('chat-header-pr-badge')).toBeNull();
    expect(getByTestId('review-toggle')).toBeTruthy();
  });

  it('hides PR + workspace +/- and shows Design Preview on a design thread', async () => {
    const pane = await buildPane(makeThread({ mode: 'design' }));
    installSubscribeMock(
      status({ openPrUrl: 'https://github.com/o/r/pull/7', openPrNumber: 7 }),
    );
    const { queryByTestId, getByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();

    expect(queryByTestId('chat-header-pr-badge')).toBeNull();
    expect(queryByTestId('review-toggle')).toBeNull();
    expect(getByTestId('design-preview-toggle')).toBeTruthy();
  });

  it('clicking the workspace +/- opens review on the workspace scope', async () => {
    const pane = await buildPane();
    setPaneLayoutItemsForTest([{ id: pane.paneId, paneId: pane.paneId, kind: 'thread', widthPx: 1 }]);
    installSubscribeMock(status({ insertions: 2, deletions: 0 }));
    const { getByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();

    expect(pane.showReviewPane).toBe(false);
    await fireEvent.click(getByTestId('review-toggle'));
    expect(pane.showReviewPane).toBe(true);
  });
});

describe('<ChatHeaderActions> subscription effect', () => {
  beforeEach(async () => {
    resetPanesForTest();
    resetEditorsForTest();
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    setBindingMock('UpdateThreadBranch', async () => makeThread());
    // The Open-in-editor control loads this catalog on mount.
    setBindingMock('ListAvailableEditors', async () => []);
    setBindingMock('GetEditorSettings', async () => ({ preference: '' }));
    await loadSettings();
  });

  it('does NOT re-subscribe when pane.replaceThread updates unrelated metadata', async () => {
    // Regression guard for the per-token flicker: the attach effect tracks the
    // $derived gitCwd, whose value-equality short-circuits re-runs when only
    // token usage / mode changed.
    const thread = makeThread({ workspacePath: '/workspace' });
    const pane = await buildPane(thread);
    const { subscribeFn } = installSubscribeMock(status({ hasChanges: true }));
    render(ChatHeaderActions, { props: { pane } });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(1);

    pane.replaceThread({ ...thread, lastTokenUsage: '{"used":1234}' });
    await flush();

    expect(subscribeFn).toHaveBeenCalledTimes(1);
    expect(getBindingMock('GitStatusUnsubscribe')).not.toHaveBeenCalled();
  });

  it('resubscribes when the workspace path changes', async () => {
    const thread = makeThread({ workspacePath: '/workspace' });
    const pane = await buildPane(thread);
    const { subscribeFn } = installSubscribeMock(status());
    render(ChatHeaderActions, { props: { pane } });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(1);

    // A worktree path supersedes workspacePath in gitCwd → new watcher.
    pane.replaceThread({ ...thread, worktreePath: '/wt/branch-a' });
    await flush();

    expect(subscribeFn).toHaveBeenCalledTimes(2);
    expect(getBindingMock('GitStatusUnsubscribe')).toHaveBeenCalledTimes(1);
  });

  it('updates the workspace +/- when a live git:status push arrives', async () => {
    // End-to-end through the mounted component: ChatHeaderActions wires
    // pane.gitStatus.attach(), the slot subscribes to 'git:status' itself, and a
    // live push flows slot → reactive status → WorkspaceDiffBadge re-render. The
    // store unit test covers the slot in isolation; this adds the component
    // mount + badge re-render path on top.
    const pane = await buildPane();
    const { id } = installSubscribeMock(status({ insertions: 1, deletions: 0 }));
    const { getByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();
    // Initial value comes from the subscribe result, not the listener.
    expect(getByTestId('workspace-diff-counts').textContent).toContain('+1');

    emitWailsEvent('git:status', {
      subscriptionId: id,
      status: status({ insertions: 99, deletions: 7 }),
    });
    await flush();

    // Only the live push (the slot's own git:status subscription) produces +99 / -7.
    expect(getByTestId('workspace-diff-counts').textContent).toContain('+99');
    expect(getByTestId('workspace-diff-counts').textContent).toContain('-7');
  });
});
