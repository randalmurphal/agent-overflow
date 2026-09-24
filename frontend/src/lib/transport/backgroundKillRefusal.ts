import { TransportError } from './wsClient';

/**
 * The client-side half of a refused Stop.
 *
 * A Claude interrupt kills every live async agent the session holds, so
 * the backend's two Stop RPCs (InterruptTurn, InterruptAndRevertIfClean)
 * refuse with `background_agents_running` while such agents are live and
 * the caller has not confirmed (internal/app/app_background_kill.go). The
 * refusal names the agents in the frame's `backgroundAgents` field, the
 * way `scope_required` names its scope: a field, because a method error's
 * prose does not survive the wire for a non-loopback caller, and this is
 * what the confirmation shows.
 *
 * The payload is untrusted input. Each element is validated here and a
 * malformed one is dropped; a refusal whose list cannot be read at all is
 * still a refusal, and answers an empty list, so the asker still asks
 * rather than stopping unconfirmed.
 */

export const BACKGROUND_AGENTS_RUNNING = 'background_agents_running';

/** Served run states a Claude interrupt kills (triage AgentRunState). */
export type BackgroundKillRunState = 'running' | 'parked';

/** One live background agent a refused Stop would have killed. */
export interface BackgroundKillAgent {
  /** The launch row its run state is served on: the Agent call, or the resume carrier. */
  launchItemId: string;
  /** The task line the agent is named by. */
  description: string;
  runState: BackgroundKillRunState;
  /** The row whose subtree is its transcript; opens its agent pane. */
  transcriptRootId: string;
}

const MAX_FIELD_LENGTH = 512;

export function isBackgroundKillRefusal(err: unknown): err is TransportError {
  return err instanceof TransportError && err.code === BACKGROUND_AGENTS_RUNNING;
}

/**
 * The agents a refused Stop named, or null when `err` is not that refusal.
 */
export function refusedBackgroundAgents(err: unknown): BackgroundKillAgent[] | null {
  if (!isBackgroundKillRefusal(err)) return null;
  const raw = err.backgroundAgents;
  if (!Array.isArray(raw)) return [];
  const agents: BackgroundKillAgent[] = [];
  for (const entry of raw) {
    const agent = readAgent(entry);
    if (agent) agents.push(agent);
  }
  return agents;
}

function readAgent(entry: unknown): BackgroundKillAgent | null {
  if (typeof entry !== 'object' || entry === null) return null;
  const record = entry as Record<string, unknown>;
  const launchItemId = readString(record.launchItemId);
  if (!launchItemId) return null;
  return {
    launchItemId,
    description: readString(record.description),
    runState: record.runState === 'parked' ? 'parked' : 'running',
    transcriptRootId: readString(record.transcriptRootId) || launchItemId,
  };
}

function readString(value: unknown): string {
  return typeof value === 'string' ? value.slice(0, MAX_FIELD_LENGTH) : '';
}
