// Fake for `bindings/agent-overflow/app.js` — the auto-generated Wails
// bindings that the real app imports via `lib/stores/bindings.ts`.
//
// Tests call `setBindingMock('ListItems', impl)` to stub specific RPC methods.
// Anything not explicitly mocked rejects with a clear error so an untested
// code path can't silently no-op.

import { vi, type Mock } from 'vitest';

type MockedFn = Mock<(...args: unknown[]) => unknown>;

const mocks: Map<string, MockedFn> = new Map();

/**
 * Install (or replace) a mock implementation for a binding.
 * Returns the underlying vi.fn so tests can inspect call args.
 */
export function setBindingMock(
  name: string,
  impl: (...args: never[]) => unknown,
): MockedFn {
  const fn = vi.fn(impl as (...args: unknown[]) => unknown);
  mocks.set(name, fn);
  return fn;
}

/**
 * Direct read for assertions (call counts, args).
 */
export function getBindingMock(name: string): MockedFn | undefined {
  return mocks.get(name);
}

/**
 * Reset every binding between tests.
 */
export function resetBindingMocks(): void {
  mocks.clear();
}

function dispatch(name: string) {
  return (...args: unknown[]) => {
    const fn = mocks.get(name);
    if (!fn) {
      throw new Error(
        `Binding ${name} called without a mock. Install one via setBindingMock('${name}', impl) in the test.`,
      );
    }
    return fn(...args);
  };
}

// Every binding re-exported from `lib/stores/bindings.ts`.
// Keep this list in sync with that file.
export const ArchiveThread = dispatch('ArchiveThread');
export const CreateThread = dispatch('CreateThread');
export const DeleteThread = dispatch('DeleteThread');
export const GetThread = dispatch('GetThread');
export const ListThreads = dispatch('ListThreads');
export const RenameThread = dispatch('RenameThread');
export const SwitchThread = dispatch('SwitchThread');

export const StartSession = dispatch('StartSession');
export const StopSession = dispatch('StopSession');
export const ReconnectSession = dispatch('ReconnectSession');
export const SendMessage = dispatch('SendMessage');
export const InterruptTurn = dispatch('InterruptTurn');
export const RespondToApproval = dispatch('RespondToApproval');

export const GetPayloadData = dispatch('GetPayloadData');
export const ListItems = dispatch('ListItems');
export const ListPayloadMetas = dispatch('ListPayloadMetas');

export const GetSettings = dispatch('GetSettings');
export const UpdateSettings = dispatch('UpdateSettings');

export const GetProviderStatuses = dispatch('GetProviderStatuses');
export const GetModelsForProvider = dispatch('GetModelsForProvider');

export const GetGitStatus = dispatch('GetGitStatus');
export const GetWorkingTreeDiff = dispatch('GetWorkingTreeDiff');
export const GitListBranches = dispatch('GitListBranches');
export const GitCommit = dispatch('GitCommit');
export const GitPush = dispatch('GitPush');
export const GitPull = dispatch('GitPull');
export const GitCheckout = dispatch('GitCheckout');
export const GitCreateBranch = dispatch('GitCreateBranch');
export const GitCreatePR = dispatch('GitCreatePR');
export const GitCreateWorktree = dispatch('GitCreateWorktree');
export const GitRemoveWorktree = dispatch('GitRemoveWorktree');

export const ListDiscussions = dispatch('ListDiscussions');
export const GetDiscussion = dispatch('GetDiscussion');
export const CreateDiscussion = dispatch('CreateDiscussion');
export const UpdateDiscussion = dispatch('UpdateDiscussion');
export const DeleteDiscussion = dispatch('DeleteDiscussion');
export const StartDiscussion = dispatch('StartDiscussion');
export const GetChannelMessages = dispatch('GetChannelMessages');
export const PostChannelMessage = dispatch('PostChannelMessage');
