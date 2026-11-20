// Which activity-run membership rule a thread's history is grouped by: the
// rule of the backend that owns it, read from that backend's hello. Its
// pages, stubs and members answers describe runs by that rule, so the pane
// groups by it too, and a request asks for it (`agentMessageMembersField`)
// exactly when the backend advertised it
// (docs/architecture/timeline-window-pages.md §1).

import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { getPinnedBackend } from '../transport/backends';
import { threadBackend } from '../transport/entityIndex';
import {
  AGENT_MESSAGES_ABSORBED_RULE,
  AGENT_MESSAGE_MEMBERS_RULE,
  type ActivityRunRule,
} from '../utils/activityRunSpans';
import { getTransportHelloFor } from './transportStatus.svelte';

/** The hello capability of a backend that groups agent messages as members. */
export const AGENT_MESSAGE_RUN_MEMBERS_CAPABILITY = 'activity-runs.agent-message-members.v1';

/**
 * The rule `backend` groups by. Reactive on its hello. A backend that has
 * sent no hello is grouped by the absorbed rule until it does, the answer
 * every capability gives before its hello.
 */
export function backendActivityRunRule(backend: BackendKey): ActivityRunRule {
  return getTransportHelloFor(backend)?.capabilities?.includes(AGENT_MESSAGE_RUN_MEMBERS_CAPABILITY)
    ? AGENT_MESSAGE_MEMBERS_RULE
    : AGENT_MESSAGES_ABSORBED_RULE;
}

/** The rule a thread's history is grouped by: its owning backend's. */
export function threadActivityRunRule(threadId: string | null | undefined): ActivityRunRule {
  return backendActivityRunRule((threadId ? threadBackend(threadId) : undefined) ?? HOME_BACKEND);
}

/**
 * The `agentMessageMembers` field of a history request about `threadId`:
 * present exactly when the backend the call goes to groups by the
 * member-on-sight rule, so a request to any other backend is the one it
 * has always been sent. Read when the call is issued.
 */
export function agentMessageMembersField(threadId: string): { agentMessageMembers?: true } {
  const backend = getPinnedBackend() ?? threadBackend(threadId) ?? HOME_BACKEND;
  return backendActivityRunRule(backend) === AGENT_MESSAGE_MEMBERS_RULE ? { agentMessageMembers: true } : {};
}
