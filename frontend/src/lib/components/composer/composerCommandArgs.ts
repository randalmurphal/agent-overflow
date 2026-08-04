// Resolving `/model <arg>` and `/effort <arg>` against the pickers' own lists.
//
// Pure, and deliberately generic over `{ id, label }` rather than over the
// model/effort types: the composer must not grow a second idea of what a model
// is. Callers project the picker's list into candidates and hand it here, so a
// name the picker cannot show is a name this cannot resolve.
//
// Matching is case-insensitive and layered, most specific first: an exact id,
// an exact label, then a substring of either. Ambiguity is an ERROR, never a
// first-match win — silently switching to whichever candidate happened to sort
// first is the failure mode this exists to prevent.

export interface ArgCandidate {
  /** The value applied on a match (a model slug, an effort tier). */
  id: string;
  /** What the picker prints for it. */
  label: string;
}

export interface ArgResolution {
  id?: string;
  /** User-facing reason, naming the closest candidates. */
  error?: string;
}

/** How many candidate names an error message lists before it stops helping. */
const MAX_SUGGESTIONS = 6;

function suggest(candidates: readonly ArgCandidate[]): string {
  const names = candidates.slice(0, MAX_SUGGESTIONS).map((candidate) => candidate.label);
  const suffix = candidates.length > MAX_SUGGESTIONS ? ', …' : '';
  return names.join(', ') + suffix;
}

/**
 * Resolve `arg` to exactly one candidate.
 *
 * `subject` names the thing in the error text ("model", "effort tier").
 */
export function resolveArgCandidate(
  arg: string,
  candidates: readonly ArgCandidate[],
  subject: string,
): ArgResolution {
  const needle = arg.trim().toLowerCase();
  if (needle === '') return { error: `Name a ${subject}. Options: ${suggest(candidates)}` };
  if (candidates.length === 0) {
    return { error: `No ${subject} options are available for this thread yet.` };
  }

  const exactId = candidates.filter((c) => c.id.toLowerCase() === needle);
  if (exactId.length === 1) return { id: exactId[0].id };

  const exactLabel = candidates.filter((c) => c.label.toLowerCase() === needle);
  if (exactLabel.length === 1) return { id: exactLabel[0].id };

  const partial = candidates.filter(
    (c) => c.id.toLowerCase().includes(needle) || c.label.toLowerCase().includes(needle),
  );
  if (partial.length === 1) return { id: partial[0].id };
  if (partial.length > 1) {
    return { error: `“${arg.trim()}” matches several ${subject}s: ${suggest(partial)}` };
  }
  return { error: `No ${subject} matches “${arg.trim()}”. Options: ${suggest(candidates)}` };
}
