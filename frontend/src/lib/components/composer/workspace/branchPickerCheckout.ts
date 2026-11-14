// The branch picker's checkout of a row in the pane's own checkout: a branch,
// or a tag, which leaves the workspace detached there.

import { GitCheckout } from '../../../stores/bindings';
import { recordBranchSelection } from '../../../stores/branchMru';
import {
  applyToDraftPlaceholdersInWorkspace,
  placeholderWorkspaceOf,
} from '../../../stores/draftWorkspaceSync';
import { warnLeftBehind } from '../../../stores/gitLeftBehind';
import { applyCheckoutState } from '../../../stores/gitStatusStore.svelte';
import { addErrorToast, addToast } from '../../../stores/toast.svelte';
import type { ThreadPane } from '../../../stores/thread.svelte';
import type { GitWorkspaceState, WorkspaceRef } from '../../../types/git';
import { sameNormalizedPath } from '../../../utils/path';
import { userFacingError } from '../../../utils/userFacingError';
import type { PickerRow } from './branchPickerRows';

type CheckoutPane = Pick<
  ThreadPane,
  'paneId' | 'thread' | 'hasDraftPlaceholder' | 'draftPlaceholder' | 'applyDraftPlaceholderWorkspace'
>;

/**
 * Checks `row` out in `ws` and reports the outcome as a toast. `ws` and the
 * placeholder identity are captured before the first await and read from the
 * captures after it: a pane switch mid-checkout re-points `pane.thread`, and
 * the answer describes `ws`, not wherever the pane moved.
 */
export async function checkoutPickerRow(
  pane: CheckoutPane,
  ws: WorkspaceRef,
  row: PickerRow,
): Promise<void> {
  const placeholderId = pane.draftPlaceholder?.id ?? '';
  try {
    const next = (await GitCheckout(ws, row.ref)) as GitWorkspaceState;
    const checkedOut = placeholderWorkspaceOf(next);
    // A tag checkout has no branch; only a branch row names one.
    if (!checkedOut.branch && row.kind === 'branch') checkedOut.branch = row.name;
    // The branch, tag or detached commit the label shows, before the status
    // stream reports it.
    applyCheckoutState(ws, { ...next, branch: checkedOut.branch });
    // A checkout happens in a DIRECTORY. Persisted thread rows in it are
    // re-branched by the backend and arrive as a thread:updated broadcast;
    // draft composers have no row for that broadcast to name, so they are
    // told here. Panes on the same project but a different worktree are
    // untouched: the checkout did not move them.
    const reached = applyToDraftPlaceholdersInWorkspace(ws, checkedOut);
    // The acting placeholder is normally one of those; apply directly when
    // it is not in the registry, guarding the placeholder identity and the
    // workspace the checkout was launched against (either can move under
    // the await).
    if (
      pane.hasDraftPlaceholder &&
      !reached.has(pane.paneId) &&
      pane.draftPlaceholder?.id === placeholderId &&
      sameNormalizedPath(pane.thread?.workspacePath ?? '', ws.workspacePath)
    ) {
      pane.applyDraftPlaceholderWorkspace(checkedOut);
    }
    // Recorded against the captured project, not the pane's current one:
    // the checkout happened in that project whatever the pane shows now.
    recordBranchSelection(ws.projectId, row.ref);
    addToast('info', `Checked out ${checkedOut.branch || next.tag || row.name}`);
    warnLeftBehind(next);
  } catch (err) {
    console.error('branch checkout failed:', err);
    addErrorToast(`Failed to checkout: ${userFacingError(err)}`, err);
  }
}
