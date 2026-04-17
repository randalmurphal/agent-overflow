// Tiny fuzzy subsequence matcher + ranker for the command palette.
//
// Scoring (higher is better):
//   +10 for each consecutive match ("camel" > "c_e_m_l")
//   +5  for a match on a word boundary (start-of-string, after space/./-/_)
//   +3  for a case-sensitive match (user typed exact case)
//   -1  per skipped character between matches
//
// Ties break on shorter target length so "new" matches "thread.new" before
// "thread.new.discussion".

export interface FuzzyMatch {
  score: number;
  /** Indices of matched characters in the target string. */
  indices: number[];
}

const WORD_BOUNDARY = /[\s._\-:/]/;

export function fuzzyMatch(query: string, target: string): FuzzyMatch | null {
  if (query.length === 0) {
    return { score: 0, indices: [] };
  }
  if (target.length === 0) return null;

  const q = query.toLowerCase();
  const t = target.toLowerCase();
  const indices: number[] = [];
  let score = 0;
  let ti = 0;
  let lastMatchIndex = -2;

  for (let qi = 0; qi < q.length; qi += 1) {
    const qc = q[qi];
    while (ti < t.length && t[ti] !== qc) {
      ti += 1;
      score -= 1;
    }
    if (ti >= t.length) return null;

    // Match found at ti.
    indices.push(ti);

    // Consecutive bonus.
    if (ti === lastMatchIndex + 1) score += 10;
    // Word boundary bonus.
    if (ti === 0 || WORD_BOUNDARY.test(target[ti - 1])) score += 5;
    // Case-sensitive match bonus.
    if (target[ti] === query[qi]) score += 3;

    lastMatchIndex = ti;
    ti += 1;
  }
  return { score, indices };
}

export interface FuzzyCandidate<T> {
  item: T;
  /** Candidate text to match against. */
  text: string;
}

export interface FuzzyResult<T> extends FuzzyMatch {
  item: T;
}

/**
 * Match a query against a set of candidates and return those that match,
 * sorted by score desc, tie-broken on shorter text.
 */
export function fuzzyFilter<T>(query: string, candidates: FuzzyCandidate<T>[]): FuzzyResult<T>[] {
  if (query.length === 0) {
    return candidates.map((c) => ({ item: c.item, score: 0, indices: [] }));
  }
  const out: FuzzyResult<T>[] = [];
  for (const c of candidates) {
    const m = fuzzyMatch(query, c.text);
    if (m) out.push({ item: c.item, score: m.score, indices: m.indices });
  }
  out.sort((a, b) => {
    if (b.score !== a.score) return b.score - a.score;
    const la = candidates.find((c) => c.item === a.item)?.text.length ?? 0;
    const lb = candidates.find((c) => c.item === b.item)?.text.length ?? 0;
    return la - lb;
  });
  return out;
}
