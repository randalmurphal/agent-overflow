// The roster editor's checks, mirroring internal/settings/agentroster.go
// (agentProfileProblem, agentNoteProblem and the list bounds) so the page
// never sends an edit the strict update path refuses.

import type { AgentNote, AgentProfile } from '../../types/settings';

export const MAX_AGENT_PROFILES = 24;
export const MAX_PROFILE_NAME_CHARS = 32;
export const MAX_PROFILE_USE_FOR_CHARS = 400;
export const MAX_PROFILE_MODEL_CHARS = 128;
export const MAX_PROFILE_INSTRUCTIONS_BYTES = 4096;
export const MAX_AGENT_NOTES = 8;
export const MAX_AGENT_NOTE_CHARS = 800;

const PROFILE_NAME = /^[\p{L}\p{N}][\p{L}\p{N} ._-]*$/u;

function chars(value: string): number {
  return [...value].length;
}

export function trimProfile(profile: AgentProfile): AgentProfile {
  return {
    name: profile.name.trim(),
    useFor: profile.useFor.trim(),
    provider: profile.provider,
    model: profile.model.trim(),
    effort: profile.effort,
    runtimeMode: profile.runtimeMode,
    instructions: profile.instructions.trim(),
  };
}

export function trimNote(note: AgentNote): AgentNote {
  return { text: note.text.trim(), when: note.when };
}

/** What is wrong with each profile, by index, as the page shows it. */
export function profileProblems(profiles: readonly AgentProfile[]): Map<number, string> {
  const problems = new Map<number, string>();
  const seen = new Set<string>();
  profiles.forEach((raw, index) => {
    const profile = trimProfile(raw);
    const problem = profileProblem(profile, seen);
    if (problem) problems.set(index, problem);
    seen.add(profile.name.toLowerCase());
  });
  return problems;
}

function profileProblem(profile: AgentProfile, seen: ReadonlySet<string>): string {
  if (profile.name === '') return 'Give the profile a name.';
  if (chars(profile.name) > MAX_PROFILE_NAME_CHARS) {
    return `A name is at most ${MAX_PROFILE_NAME_CHARS} characters.`;
  }
  if (!PROFILE_NAME.test(profile.name)) {
    return 'A name may use only letters, digits, spaces, dots, dashes and underscores, and starts with a letter or digit.';
  }
  if (seen.has(profile.name.toLowerCase())) return `Another profile is already named ${profile.name}.`;
  if (profile.useFor === '') return 'Say what the profile is for: agents choose a profile by it.';
  if (chars(profile.useFor) > MAX_PROFILE_USE_FOR_CHARS) {
    return `"Use for" is at most ${MAX_PROFILE_USE_FOR_CHARS} characters.`;
  }
  if (chars(profile.model) > MAX_PROFILE_MODEL_CHARS) {
    return `A model is at most ${MAX_PROFILE_MODEL_CHARS} characters.`;
  }
  if (profile.model !== '' && profile.provider === '') {
    return 'A model needs the provider that runs it.';
  }
  if (new TextEncoder().encode(profile.instructions).length > MAX_PROFILE_INSTRUCTIONS_BYTES) {
    return `Instructions are at most ${MAX_PROFILE_INSTRUCTIONS_BYTES} bytes.`;
  }
  return '';
}

/** What is wrong with each note, by index. */
export function noteProblems(notes: readonly AgentNote[]): Map<number, string> {
  const problems = new Map<number, string>();
  notes.forEach((raw, index) => {
    const note = trimNote(raw);
    if (note.text === '') problems.set(index, 'Write the note, or delete it.');
    else if (chars(note.text) > MAX_AGENT_NOTE_CHARS) {
      problems.set(index, `A note is at most ${MAX_AGENT_NOTE_CHARS} characters.`);
    }
  });
  return problems;
}
