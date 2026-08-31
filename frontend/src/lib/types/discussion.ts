// Discussion types — mirrors the Go structs in internal/store/discussion_types.go
// and internal/discussion/*.go. Read those files (not these comments) if the
// backend shapes drift.

export type DiscussionScope = 'global' | 'project';

export interface DiscussionParticipant {
  role: string;
  description: string;
  system: string;
  // Per-participant overrides. Empty/undefined = inherit from parent thread.
  // Key is always present (generated Wails models use `string | undefined`,
  // not `?:`), so we follow the same shape.
  provider: string | undefined;
  model: string | undefined;
}

export interface DiscussionSettings {
  maxTurns: number;
}

/**
 * Persisted discussion definition. `id`, `createdAt`, and `updatedAt` are assigned
 * by the backend on create. The frontend sends them blank and they come back
 * populated from `GetDiscussion`/`ListDiscussions`.
 */
export interface DiscussionDefinition {
  id: string;
  name: string;
  description: string;
  scope: DiscussionScope;
  // projectId is present in the generated Wails class with type
  // `string | undefined`, so we mirror that exact shape for assignability.
  projectId: string | undefined;
  participants: DiscussionParticipant[];
  settings: DiscussionSettings;
  createdAt: number;
  updatedAt: number;
}

export type ChannelFromType = 'agent' | 'human' | 'system' | string;

/**
 * One ordered message in a channel. `sequence` is the monotonic ordering
 * assigned by the backend. `fromRole` is present when `fromType` is `agent`
 * and identifies which participant spoke. `meta` is the optional JSON
 * sidecar populated at PostMessage time; today it carries the
 * `pathRefs` allowlist the markdown linkifier consumes.
 */
export interface ChannelMessage {
  id: string;
  channelId: string;
  sequence: number;
  fromType: ChannelFromType;
  fromId: string;
  fromRole: string | undefined;
  content: string;
  meta?: string;
  createdAt: number;
}

/**
 * One entry in ChannelStatePayload's participants roster. Mirrors Go's
 * ChannelParticipantState (app_discussion.go).
 */
export interface ChannelParticipantState {
  threadId: string;
  role: string;
  provider: string;
  model: string;
  // True when this participant's latest channel post carried a CONCLUDE
  // marker (see internal/discussion/conclusion.go). Only meaningful on
  // the live-FSM branch of buildChannelState; a concluded/non-open
  // channel's SQLite-fallback branch always reports false.
  proposedConclusion: boolean;
}

/**
 * The discussion:state wire payload and GetChannelState's return shape —
 * a snapshot of the deliberation FSM plus enough participant metadata to
 * render "whose turn is it" without a second round-trip. Mirrors Go's
 * ChannelStatePayload (app_discussion.go). `status` mirrors
 * `Channel['status']` above but is declared independently rather than
 * imported from it — the two happen to share the same wire values today,
 * not because one is a specialization of the other.
 */
export interface ChannelStatePayload {
  channelId: string;
  threadId: string;
  status: 'open' | 'concluded' | 'closed' | string;
  turnCount: number;
  maxTurns: number;
  awaitingResponse: boolean;
  currentSpeakerThreadId: string;
  currentSpeakerRole: string;
  participants: ChannelParticipantState[];
}

/**
 * Mirrors Go's discussion.DefaultMaxTurns (internal/discussion/
 * deliberation.go) — the shared circuit-breaker turn count. A third
 * copy exists as the frozen `DEFAULT 8` SQL literal in the v12
 * migration (internal/store/migrate.go); that one never changes even
 * if this default moves, per migration semantics. Keep this constant
 * in lockstep with the Go constant.
 */
export const DEFAULT_MAX_TURNS = 8;

/**
 * Returns a blank draft suitable for DiscussionEditor to populate.
 * Includes two default participants so the UX is usable without scrolling a
 * long "add participant" chain.
 */
export function createEmptyDiscussionDefinition(): DiscussionDefinition {
  return {
    id: '',
    name: '',
    description: '',
    scope: 'global',
    projectId: '',
    participants: [
      { role: 'advocate', description: 'Argues for the current direction.', system: '', provider: undefined, model: undefined },
      { role: 'critic', description: 'Presses on weak spots and risks.', system: '', provider: undefined, model: undefined },
    ],
    settings: { maxTurns: DEFAULT_MAX_TURNS },
    createdAt: 0,
    updatedAt: 0,
  };
}
