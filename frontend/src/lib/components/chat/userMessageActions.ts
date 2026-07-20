import type { Item } from '../../types/models';

export interface UserMessageActions {
  onForkMessage?: (item: Item) => void | Promise<void>;
  forkingItemId?: string | null;
}
