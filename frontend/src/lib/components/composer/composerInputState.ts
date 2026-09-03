export interface ComposerInputStateInput {
  isDisabled: boolean;
  /** See ComposerSendStateInput.sendUngranted. */
  sendUngranted?: boolean;
  /**
   * The display name of the thread's machine when this client cannot
   * reach it, else empty. An attached backend dropping disables the
   * composer for its threads in place (spec §10: never a silent failover
   * to another machine); the page's own backend dropping is the
   * transport banner's job and never sets this.
   */
  unreachableTarget?: string;
  /**
   * True when this client's socket to the thread's own backend is down and
   * there is nothing local to fall back on.
   *
   * Distinct from `unreachableTarget`, which names ANOTHER machine. This is
   * the page's own backend, and it is deliberately not set for the embedded
   * desktop webview: that window is on the machine, its outage is the
   * transport banner's story, and disabling its composer would change a
   * surface nobody asked to change. It IS set for the phone shell,
   * `--connect` and a remote browser, where "disconnected" means the send
   * cannot go anywhere and there is no cross-disconnect queue to hold it
   * (docs/specs/remote-access.md, "Pairing and remote-only").
   */
  offline?: boolean;
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
      Boolean(input.sendUngranted) ||
      Boolean(input.unreachableTarget) ||
      Boolean(input.offline) ||
      input.hasBlockingPrompt,
    value: input.hasUserInputPrompt ? input.userInputCustomAnswer : input.draftContent,
    placeholder: inputPlaceholder(input),
  };
}

function inputPlaceholder(input: ComposerInputStateInput): string {
  if (input.isDisabled) return 'Select or create a thread to start';
  // Read before the prompt cases: a session that cannot send also cannot
  // answer, so offering the prompt's instructions would be a dead end.
  if (input.sendUngranted) return 'This device has read-only access';
  // Named machine first: "<name> is unreachable" says more than "offline"
  // and both are true at once when another machine's socket is the one
  // that dropped.
  if (input.unreachableTarget) return `${input.unreachableTarget} is unreachable`;
  if (input.offline) return 'Disconnected from the agent backend';
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
