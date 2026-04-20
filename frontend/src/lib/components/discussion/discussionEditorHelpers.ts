// Helper functions for DiscussionEditor.svelte.
//
// Extracted to keep the component under 300 lines. Each helper is pure
// TypeScript — no runes, no fetches. The editor still owns all reactive
// state; these functions just operate on draft snapshots.

import { DEFAULT_MAX_TURNS } from '../../types/discussion';
import type {
  DiscussionDefinition,
  DiscussionParticipant,
  DiscussionScope,
} from '../../types/discussion';

export function cloneDef(def: DiscussionDefinition): DiscussionDefinition {
  return {
    ...def,
    participants: def.participants.map((p) => ({ ...p })),
    settings: { ...def.settings },
  };
}

/**
 * Immutably update one top-level field on a DiscussionDefinition. Returns a
 * new object so draft changes remain reactive.
 */
export function setDefField<K extends keyof DiscussionDefinition>(
  def: DiscussionDefinition,
  key: K,
  value: DiscussionDefinition[K],
): DiscussionDefinition {
  return { ...def, [key]: value };
}

/**
 * Flip the scope between global + project. Clears projectId when leaving
 * project scope so a stale path can't round-trip to the server.
 */
export function setScope(
  def: DiscussionDefinition,
  scope: DiscussionScope,
): DiscussionDefinition {
  return {
    ...def,
    scope,
    projectId: scope === 'project' ? def.projectId ?? '' : '',
  };
}

/** Parse a raw text input into a valid maxTurns value. */
export function setMaxTurns(def: DiscussionDefinition, raw: string): DiscussionDefinition {
  const parsed = parseInt(raw, 10);
  const maxTurns = Number.isFinite(parsed) && parsed > 0 ? parsed : DEFAULT_MAX_TURNS;
  return { ...def, settings: { ...def.settings, maxTurns } };
}

export function updateParticipant(
  def: DiscussionDefinition,
  index: number,
  next: DiscussionParticipant,
): DiscussionDefinition {
  return {
    ...def,
    participants: def.participants.map((p, i) => (i === index ? next : p)),
  };
}

export function addParticipant(def: DiscussionDefinition): DiscussionDefinition {
  return {
    ...def,
    participants: [
      ...def.participants,
      { role: '', description: '', system: '', provider: undefined, model: undefined },
    ],
  };
}

export function removeParticipant(def: DiscussionDefinition, index: number): DiscussionDefinition {
  if (def.participants.length <= 2) return def;
  return {
    ...def,
    participants: def.participants.filter((_, i) => i !== index),
  };
}

/**
 * Mirror of the Go-side `normalizeDiscussionDefinition` validation so we
 * can surface errors inline instead of round-tripping to the backend.
 * Keep these in sync with `internal/discussion/registry.go`.
 */
export function validateDiscussion(draft: DiscussionDefinition): string | null {
  if (!draft.name.trim()) return 'Discussion name is required.';
  if (draft.participants.length < 2) return 'A discussion needs at least 2 participants.';
  if (draft.scope === 'project' && !(draft.projectId ?? '').trim()) {
    return 'Project-scoped discussions require a project path.';
  }
  for (let i = 0; i < draft.participants.length; i++) {
    const p = draft.participants[i];
    if (!p.role.trim()) return `Participant ${i + 1} needs a role.`;
    if (!p.system.trim()) return `Participant ${i + 1} needs a system prompt.`;
  }
  if (!Number.isInteger(draft.settings.maxTurns) || draft.settings.maxTurns < 1) {
    return 'Max turns must be a positive integer.';
  }
  return null;
}
