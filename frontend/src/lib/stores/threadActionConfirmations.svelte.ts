import type { ThreadActionCtx } from '../components/sidebar/threadRowActions';

export type ThreadActionConfirmationKind = 'archive' | 'delete';

export interface ThreadActionConfirmation {
  kind: ThreadActionConfirmationKind;
  ctx: ThreadActionCtx;
}

let pendingConfirmation: ThreadActionConfirmation | null = $state(null);

export function getPendingThreadActionConfirmation(): ThreadActionConfirmation | null {
  return pendingConfirmation;
}

export function requestThreadActionConfirmation(
  kind: ThreadActionConfirmationKind,
  ctx: ThreadActionCtx,
): void {
  pendingConfirmation = { kind, ctx };
}

export function clearThreadActionConfirmation(): void {
  pendingConfirmation = null;
}

export function resetThreadActionConfirmationsForTest(): void {
  pendingConfirmation = null;
}
