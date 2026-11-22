import type { SendButtonAction } from './toolbar/sendButtonTypes';
import type { ScopeGrant } from '../../transport/scopes';

export interface ComposerSendStateInput {
  isDisabled: boolean;
  /**
   * Whether this session holds `threads:operate` on the thread's computer
   * (`threadScopeGrant`); absent means granted. Anything but `granted`
   * means nothing the send button does can reach the backend. `refused`
   * is a known grant without it; `unknown` is a computer whose grant has
   * not arrived in this window. Separate from `isDisabled`, which is
   * "there is no thread to send into": they want different placeholder
   * copy, and this one is a property of the session rather than the pane.
   */
  sendGrant?: ScopeGrant;
  /**
   * The thread is the hidden copy an agent's question runs in
   * (`Thread.askCopy`): it takes no message from a person, and the
   * backend refuses one.
   */
  askCopy?: boolean;
  sending: boolean;
  hasBlockingPrompt: boolean;
  hasUserInputPrompt: boolean;
  hasDraftContent: boolean;
  hasPlanSource: boolean;
  hasDraftPlanComments: boolean;
  hasDiffReviewSource: boolean;
  hasDraftDiffReviewComments: boolean;
  isTurnActive: boolean;
  /**
   * A thread-level operation owns sending right now — the edit-and-resend
   * saga, which reverts and re-sends under one backend lock. Distinct
   * from `sending` (this composer's own send) because nothing here
   * started it and nothing here can stop it; the button just refuses.
   */
  sendSuspended?: boolean;
  /**
   * The draft is a shell command (a leading `!`): `ready` to run, `empty`
   * while no command follows the `!`, `blocked` while it cannot run. Send
   * then means Run, and nothing else the draft holds is sent with it.
   */
  shell?: 'ready' | 'empty' | 'blocked';
}

export interface ComposerSendState {
  canSend: boolean;
  action: SendButtonAction;
  label: string | undefined;
  hasPlanImplementAction: boolean;
}

export function deriveComposerSendState(input: ComposerSendStateInput): ComposerSendState {
  const hasPlanCommentAction = input.hasPlanSource && input.hasDraftPlanComments;
  const hasDiffReviewCommentAction = input.hasDiffReviewSource && input.hasDraftDiffReviewComments;
  const hasPlanImplementAction = input.hasPlanSource
    && !input.hasDraftContent
    && !input.hasDraftPlanComments;
  if (input.shell) {
    return {
      canSend: input.shell === 'ready'
        && !input.isDisabled
        && sendGranted(input)
        && !input.askCopy
        && !input.sending
        && !input.sendSuspended
        && !input.hasBlockingPrompt
        && !input.hasUserInputPrompt,
      action: 'run',
      label: sendLabel('run'),
      hasPlanImplementAction: false,
    };
  }
  const action = sendAction(input, {
    hasPlanCommentAction,
    hasDiffReviewCommentAction,
  });

  return {
    // Drafts and comment sends are queueable mid-round. Implement-only
    // still requires an idle turn because it runs the plan implementation
    // helper, not just a SendMessage RPC.
    canSend: !input.isDisabled
      && sendGranted(input)
      && !input.askCopy
      && !input.sending
      && !input.sendSuspended
      && !input.hasBlockingPrompt
      && !input.hasUserInputPrompt
      && (
        input.hasDraftContent
        || hasPlanCommentAction
        || hasDiffReviewCommentAction
        || (!input.isTurnActive && hasPlanImplementAction)
      ),
    action,
    label: sendLabel(action),
    hasPlanImplementAction,
  };
}

function sendGranted(input: ComposerSendStateInput): boolean {
  return (input.sendGrant ?? 'granted') === 'granted';
}

function sendLabel(action: SendButtonAction): string | undefined {
  if (action === 'send-comments') return 'Send comments';
  if (action === 'implement') return 'Implement';
  if (action === 'refine') return 'Refine';
  if (action === 'run') return 'Run';
  return undefined;
}

function sendAction(
  input: ComposerSendStateInput,
  normalized: {
    hasPlanCommentAction: boolean;
    hasDiffReviewCommentAction: boolean;
  },
): SendButtonAction {
  if (normalized.hasDiffReviewCommentAction) return 'send-comments';
  if (!input.hasPlanSource || input.isTurnActive) return 'send';
  if (normalized.hasPlanCommentAction) return 'send-comments';
  if (!input.hasDraftContent) return 'implement';
  return 'refine';
}
