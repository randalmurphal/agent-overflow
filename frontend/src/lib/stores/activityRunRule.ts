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
 * The rule a history request about `threadId` is grouped by: the rule of
 * the backend the call goes to. Read once per request, when it is issued;
 * the request's field (`agentMessageMembersField`) and the label its
 * answer carries (`RuledRead` in bindings.ts) both come from this value.
 */
export function requestActivityRunRule(threadId: string): ActivityRunRule {
  return backendActivityRunRule(getPinnedBackend() ?? threadBackend(threadId) ?? HOME_BACKEND);
}

/**
 * The `agentMessageMembers` field of a request grouped by `rule`: present
 * exactly for the member-on-sight rule, so a request to a backend without
 * the capability is the one it has always been sent.
 */
export function agentMessageMembersField(rule: ActivityRunRule): { agentMessageMembers?: true } {
  return rule === AGENT_MESSAGE_MEMBERS_RULE ? { agentMessageMembers: true } : {};
}
