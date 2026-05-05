import type { Item } from '../../types/models';

export interface UserMessageActions {
  onRevertMessage?: (item: Item) => void | Promise<void>;
  onForkMessage?: (item: Item) => void | Promise<void>;
  revertingItemId?: string | null;
  forkingItemId?: string | null;
}
