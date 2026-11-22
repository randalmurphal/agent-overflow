import type { ControlAccess } from '../../transport/scopes';

export interface ComposerInputStateInput {
  isDisabled: boolean;
  /** See ComposerSendStateInput.sendAccess. */
  sendAccess?: ControlAccess;
  /** See ComposerSendStateInput.askCopy. */
  askCopy?: boolean;
  /**
   * The display name of the thread's machine when this client cannot
   * reach it on a client with several computers, else empty. It names the
   * computer when `sendAccess` is `offline`; without it the page's own
   * backend is the one that dropped (the phone shell, `--connect`, a
   * remote browser), and there is no cross-disconnect queue to hold a
   * message (docs/specs/remote-access.md, "Pairing and remote-only").
   */
  unreachableTarget?: string;
  /**
   * Compact layout. Only the placeholder cares: Return inserts a newline on
   * a phone and Send is a button, so the desktop's chord hint would name a
   * key the on-screen keyboard does not have.
   */
  compact?: boolean;
  hasBlockingPrompt: boolean;
  hasUserInputPrompt: boolean;
  userInputCustomAnswer: string;
  draftContent: string;
  hasDiffReviewSource: boolean;
  hasDraftDiffReviewComments: boolean;
  hasPlanSource: boolean;
  hasDraftPlanComments: boolean;
}

export interface ComposerInputState {
  disabled: boolean;
  value: string;
  placeholder: string;
}

export function deriveComposerInputState(input: ComposerInputStateInput): ComposerInputState {
  return {
    disabled:
      input.isDisabled ||
      (input.sendAccess ?? 'granted') !== 'granted' ||
      Boolean(input.askCopy) ||
      input.hasBlockingPrompt,
    value: input.hasUserInputPrompt ? input.userInputCustomAnswer : input.draftContent,
    placeholder: inputPlaceholder(input),
  };
}

function inputPlaceholder(input: ComposerInputStateInput): string {
  if (input.isDisabled) return 'Select or create a thread to start';
  // Read before the prompt cases: a session that cannot send also cannot
  // answer, so offering the prompt's instructions would be a dead end.
  // Only a known grant says so: a computer whose grant has not arrived is
  // named below, as offline, which is what keeps it from answering.
  if (input.sendAccess === 'refused') return 'This device has read-only access';
  if (input.askCopy) return "An agent's question runs in this copy. It takes no messages.";
  // Named computer first: "<name> is offline" says more than "disconnected"
  // and both are true at once when another computer's socket is the one
  // that dropped. "Offline" is the one word every surface uses for a down
  // socket (Settings, the computer picker, the transport banner).
  if (input.sendAccess === 'offline') {
    return input.unreachableTarget ? `${input.unreachableTarget} is offline` : 'Disconnected from the agent backend';
  }
  // Reachable, and its grant has not arrived yet.
  if (input.sendAccess === 'pending') return 'Waiting for this thread’s computer to answer';
  // Several computers, and none is known to hold the thread.
  if (input.sendAccess === 'unrouted') return 'Waiting to learn which computer holds this thread';
  if (input.hasBlockingPrompt) return 'Respond to the approval request to continue';
  if (input.hasUserInputPrompt) return 'Type a custom answer, or choose an option above';
  if (input.hasDiffReviewSource && input.hasDraftDiffReviewComments) {
    return 'Add optional notes, or send the diff comments';
  }
  if (input.hasPlanSource && input.hasDraftPlanComments) {
    return 'Add optional notes, or send the plan comments';
  }
  if (input.hasPlanSource) return 'Add feedback to refine the plan, or leave blank to implement it';
  // The chord hint is desktop-only: compact sends with the button and
  // Return inserts a newline (`ComposerInputSurface`'s `enterSends`), so
  // naming Shift+Enter on a phone points at a key that is not there.
  if (input.compact) return 'Send a message… (@ to mention a file)';
  return 'Send a message… (Shift+Enter for newline, @ to mention a file)';
}
