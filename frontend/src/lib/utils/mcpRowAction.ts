// The MCP menu row's trailing action — which remedy fits the row's state.
//
// One decision point, pure, so the precedence is testable without mounting
// the menu. Precedence, most- to least-specific remedy:
//
//   1. needs-auth        → Sign in       (no usable credential; OAuth flow)
//   2. failed + oAuth    → Sign in again (a credential EXISTS but the
//      connection failed; Codex deterministically omits
//      `failureReason: reauthenticationRequired` for the common
//      revoked-refresh-token case, so the failed state itself must offer
//      the sign-in. The row shows the real error beside it — the label is
//      an offer of the likely fix, not a diagnosis.)
//   3. live-session row  → Reconnect     (re-run the connection in place)
//   4. anything else     → Refresh       (ephemeral status re-check)
//
// Cases 1 and 2 are one BEHAVIOR (the OAuth flow) under two labels, so
// they share the 'sign-in' kind — the label carries the difference, and
// the caller switches on kind alone.
//
// `canReconnect` is the caller's judgment (row came from THIS pane's live
// session — see mcpRowsSourceThreadId), not derivable from the row alone.
import type { ThreadMCPServer } from '../stores/bindings';

export type McpRowActionKind = 'sign-in' | 'reconnect' | 'refresh';

export interface McpRowActionSpec {
  kind: McpRowActionKind;
  /** Visible button text. */
  label: string;
  /** Tooltip and accessible name, naming the server. */
  title: string;
}

export function mcpRowAction(row: ThreadMCPServer, canReconnect: boolean): McpRowActionSpec {
  if (row.status === 'needs-auth') {
    return { kind: 'sign-in', label: 'Sign in', title: `Sign in to ${row.name}` };
  }
  if (row.status === 'failed' && row.authStatus === 'oAuth') {
    return { kind: 'sign-in', label: 'Sign in again', title: `Sign in to ${row.name} again` };
  }
  if (canReconnect) {
    return { kind: 'reconnect', label: 'Reconnect', title: `Reconnect ${row.name}` };
  }
  return { kind: 'refresh', label: 'Refresh', title: `Re-check ${row.name}` };
}
