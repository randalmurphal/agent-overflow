// What every composer action that calls the thread's computer may do now.
// The composer has one answer per capability, from one rule: the grant on
// the thread's computer, masked by `offline` while that computer cannot be
// reached. Send, Stop, Run, the approval and question answers,
// and an attachment's paste, drop or pick each read their entry here and
// issue no call unless it is `granted`. A draft is never cleared by a
// refusal here: nothing was sent.
import type { ControlAccess, EntityScopeGrant, Scope } from '../../transport/scopes';

export type ComposerCapability = Extract<Scope, 'threads:operate' | 'approvals:respond' | 'attachments:write' | 'terminal:operate'>;

export interface ComposerAccess {
  /** Send, queue, Stop, and implementing a plan (`threads:operate`). */
  send: ControlAccess;
  /** Answering an approval or a question (`approvals:respond`). */
  respond: ControlAccess;
  /** Uploading an attachment (`attachments:write`). */
  attach: ControlAccess;
  /** Running a shell command from the draft (`terminal:operate`). */
  shell: ControlAccess;
}

/**
 * The composer's access. `reachable` is false while the thread's computer
 * cannot be reached from this page. A known refusal outranks the outage:
 * reaching the computer again would not let the action through.
 */
export function composerAccess(reachable: boolean, grant: (scope: ComposerCapability) => EntityScopeGrant): ComposerAccess {
  const of = (scope: ComposerCapability): ControlAccess => {
    const answer = grant(scope);
    return answer === 'refused' || reachable ? answer : 'offline';
  };
  return {
    send: of('threads:operate'),
    respond: of('approvals:respond'),
    attach: of('attachments:write'),
    shell: of('terminal:operate'),
  };
}
