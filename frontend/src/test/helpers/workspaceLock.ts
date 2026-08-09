import type { WorkspaceChangeLockState } from '../../lib/stores/workspaceChangeLock.svelte';

// Inert workspace-change lock for component tests. Override `locked` /
// `reason` to simulate an active turn or running background tasks.
export function makeWorkspaceLock(
  overrides: Partial<WorkspaceChangeLockState> = {},
): WorkspaceChangeLockState {
  return {
    locked: false,
    reason: '',
    runningBackgroundCount: 0,
    refresh: () => {},
    ...overrides,
  };
}
