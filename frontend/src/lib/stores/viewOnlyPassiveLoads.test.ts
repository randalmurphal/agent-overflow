// The passive half of view-only mode: a load that runs because a pane
// mounted, not because anybody pressed anything, must not issue an RPC
// whose scope this session was not granted.
//
// One suite rather than a case per store, because the rule is one rule and
// the failure it guards is a BURST — the owner's live test saw a toast per
// surface on every thread open. A store that grows a new passive load gets
// a line here; a store that loses its guard fails here rather than in
// somebody's browser.
//
// Each case asserts BOTH directions. A guard that never fires would pass a
// one-sided test while having broken the local page.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { pairViewOnly, resetToLocalPage } from '../../test/helpers/scopes';
import {
  hydrateBrowserCompanionState,
  resetBrowserCompanionForTest,
} from './browserCompanion.svelte';
import { attachGitStatus, refreshGitStatus } from './gitStatusStore.svelte';
import type { WorkspaceRef } from '../types/git';
import { attachMcpServers } from './mcpServers.svelte';
import { ensureProviderModels, resetProviderModelsForTest } from './providerModels.svelte';
import { hydrateWorktreeSetup } from './worktreeSetup.svelte';
import { getUpdateState, resetForTest as resetUpdatesForTest, runUpdateCheck } from './updates.svelte';

const WORKSPACE = '/workspace';
const THREAD = 'thread-1';
const WORKSPACE_REF: WorkspaceRef = { projectId: 'project-1', workspacePath: WORKSPACE };

function stubBindings() {
  return {
    gitStatus: setBindingMock('GitStatusSubscribe', async () => ({ id: 'sub-1', status: null })),
    gitUnsubscribe: setBindingMock('GitStatusUnsubscribe', async () => undefined),
    gitRefresh: setBindingMock('GetGitStatus', async () => null),
    mcp: setBindingMock('ListThreadMcpServers', async () => []),
    models: setBindingMock('GetModelsForProvider', async () => []),
    worktreeSetup: setBindingMock('GetThreadWorktreeSetup', async () => null),
    browserCompanion: setBindingMock('BrowserCompanionThreadState', async () => ({
      threadId: THREAD,
      pages: [],
      visible: false,
    })),
    updateCheck: setBindingMock('CheckForUpdate', async () => ({
      supported: true,
      updateAvailable: false,
      currentVersion: '1.0.0',
    })),
  };
}

// The loads are fire-and-forget: an entity store's source runs on a
// microtask after attach, so a synchronous assertion would pass before the
// RPC that should not have happened had a chance to happen.
function settle(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

describe('view-only sessions issue no passive operate RPCs', () => {
  let bindings: ReturnType<typeof stubBindings>;

  beforeEach(() => {
    bindings = stubBindings();
    resetProviderModelsForTest();
    resetUpdatesForTest();
    resetBrowserCompanionForTest();
  });

  afterEach(() => {
    resetToLocalPage();
    resetProviderModelsForTest();
    resetBrowserCompanionForTest();
  });

  it('does not subscribe git status without git:operate', async () => {
    await pairViewOnly();
    const handle = attachGitStatus(WORKSPACE, { workspace: WORKSPACE_REF });
    await refreshGitStatus(WORKSPACE, WORKSPACE_REF, () => WORKSPACE);
    await settle();
    expect(bindings.gitStatus).not.toHaveBeenCalled();
    expect(bindings.gitRefresh).not.toHaveBeenCalled();
    // The inert attachment still answers, so a consumer reads an empty
    // workspace rather than crashing on a null handle.
    expect(handle.current).toBeNull();
    expect(handle.error).toBeNull();
    handle.release();
  });

  it('subscribes git status on the local page', async () => {
    const handle = attachGitStatus(WORKSPACE, { workspace: WORKSPACE_REF });
    await settle();
    expect(bindings.gitStatus).toHaveBeenCalled();
    handle.release();
  });

  it('does not list MCP servers without settings:write', async () => {
    await pairViewOnly();
    const handle = attachMcpServers('claude:' + WORKSPACE, {
      provider: 'claude',
      threadId: THREAD,
      workspacePath: WORKSPACE,
    });
    await settle();
    expect(bindings.mcp).not.toHaveBeenCalled();
    expect(handle.current).toBeNull();
    handle.release();
  });

  it('does not fetch the model catalog without threads:operate', async () => {
    await pairViewOnly();
    expect(await ensureProviderModels('claude')).toEqual([]);
    expect(bindings.models).not.toHaveBeenCalled();
  });

  it('fetches the model catalog on the local page', async () => {
    await ensureProviderModels('claude');
    expect(bindings.models).toHaveBeenCalled();
  });

  it('does not read the worktree setup snapshot without terminal:operate', async () => {
    await pairViewOnly();
    await hydrateWorktreeSetup(THREAD);
    expect(bindings.worktreeSetup).not.toHaveBeenCalled();
  });

  it('does not read the browser companion snapshot off the host', async () => {
    await pairViewOnly();
    hydrateBrowserCompanionState(THREAD);
    await settle();
    expect(bindings.browserCompanion).not.toHaveBeenCalled();
  });

  it('reads the browser companion snapshot on the local page', async () => {
    hydrateBrowserCompanionState(THREAD);
    await settle();
    expect(bindings.browserCompanion).toHaveBeenCalled();
  });

  it('does not run the launch update check off-host, and rests as unsupported', async () => {
    await pairViewOnly();
    await runUpdateCheck();
    expect(bindings.updateCheck).not.toHaveBeenCalled();
    expect(getUpdateState().supported).toBe(false);
    expect(getUpdateState().phase).toBe('idle');
  });
});
