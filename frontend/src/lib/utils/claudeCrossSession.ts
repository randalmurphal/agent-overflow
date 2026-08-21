// Claude Code's cross-session peer inbox, frontend half.
//
// MIRRORS internal/settings/claudecrosssession.go. Not enforcement — the
// backend refuses a bad value on the patch path — this exists so the field
// can explain a choice before the write, and so the two halves of one
// setting are always patched together.

import type { ClaudeCrossSession, ClaudeCrossSessionInbound } from '../types/settings';
import type { AxisOption } from './claudeSessionAxes';

// Claude Code's own schema offers a third value, `hold`. It is deliberately
// absent here and refused by the backend: a held message waits for an
// approval, and a session Agent Overflow drives has no surface to give one.
// Spike-verified on 2.1.237 — an explicitly held message parks until the
// process exits, and the mode-parity default drops it after a timeout, both
// with nothing on the wire to say so.
export const CLAUDE_CROSS_SESSION_INBOUND_OPTIONS: AxisOption<ClaudeCrossSessionInbound>[] = [
  {
    value: 'accept',
    label: 'Accept',
    description:
      'A peer message arrives as a new turn, and Claude answers it — even while you are away from the thread.',
  },
  {
    value: 'refuse',
    label: 'Ignore',
    description:
      'The thread stays listed for other sessions, but nothing they send reaches it. The sender is not told.',
  },
];

/** Mirrors ClaudeCrossSession.EffectiveInbound: enabled is never empty. */
export function effectiveInbound(cross: ClaudeCrossSession): ClaudeCrossSessionInbound | '' {
  if (!cross.enabled) return '';
  return cross.inbound || 'accept';
}

/**
 * Builds the whole setting for a patch. One key holds both halves, so
 * patching one alone would drop the other — the same rule the subagent
 * limits and the thinking axis follow.
 *
 * Turning the feature ON resolves the policy rather than leaving it empty:
 * an enabled-but-unset session falls into Claude Code's mode-parity path,
 * whose hold outcome is the silent drop this UI never wants to produce.
 */
export function crossSessionPatch(
  enabled: boolean,
  inbound: ClaudeCrossSessionInbound | '',
): ClaudeCrossSession {
  if (!enabled) {
    // The stored policy is KEPT while disabled so turning the feature back
    // on does not silently lose the user's choice.
    return inbound ? { inbound } : {};
  }
  return { enabled: true, inbound: inbound || 'accept' };
}
