import type { Item } from '../../types/models';
import type { RevertMode } from '../../types/checkpoint';
import type { PatchFile } from '../../utils/patchFiles';

export interface UserMessageActions {
  onRevertMessage?: (item: Item) => void | Promise<void>;
  onConfirmRevertMessage?: (mode: RevertMode) => void | Promise<void>;
  onCancelRevertMessage?: () => void;
  onForkMessage?: (item: Item) => void | Promise<void>;
  revertTargetItemId?: string | null;
  revertAffectedFiles?: PatchFile[];
  revertingItemId?: string | null;
  forkingItemId?: string | null;
}
