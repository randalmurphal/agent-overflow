import type { Item } from '../../types/models';

export interface UserMessageActions {
  onForkMessage?: (item: Item) => void | Promise<void>;
  forkingItemId?: string | null;
  // Revert the CURRENT thread in place back to this message: truncate
  // everything after it and drop the message text back into the composer.
  // Distinct from fork (which clones into a new thread). Undefined when the
  // provider doesn't support it, which drops the button (UserMessage derives
  // `canRequestRevert` from `typeof onRevertMessage === 'function'`).
  onRevertMessage?: (item: Item) => void | Promise<void>;
  // Anchor item of the active revert flow, non-null for its WHOLE
  // lifecycle (preflight count RPC → confirm dialog → destructive RPC),
  // not just the RPC. UserMessage disables every revert button while
  // any flow is active — one revert at a time, and a disabled control
  // beats a silently swallowed click.
  revertingItemId?: string | null;
}
