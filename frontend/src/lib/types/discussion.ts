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

/**
 * Discussion channel. The backend sets status to `open` at creation,
 * flips to `concluded` when the deliberation engine decides to stop,
 * and `closed` if explicitly closed.
 */
export interface Channel {
  id: string;
  threadId: string;
  type: string;
  status: 'open' | 'concluded' | 'closed' | string;
  createdAt: number;
  updatedAt: number;
}

export type ChannelFromType = 'agent' | 'human' | string;

/**
 * One ordered message in a channel. `sequence` is the monotonic ordering
 * assigned by the backend. `fromRole` is present when `fromType` is `agent`
 * and identifies which participant spoke.
 */
export interface ChannelMessage {
  id: string;
  channelId: string;
  sequence: number;
  fromType: ChannelFromType;
  fromId: string;
  fromRole: string | undefined;
  content: string;
  createdAt: number;
}

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
