/** A project's display label, split so renderers can dim the
 *  disambiguating parent-path prefix independently of the name. */
export interface ProjectLabel {
  /** Parent-dir segments (joined with '/', no trailing slash) that make
   *  this project distinguishable from same-named siblings. Empty when
   *  the name is unique across the given set. */
  prefix: string;
  name: string;
}

/** Flat single-string form for surfaces that can't style the prefix
 *  separately (`<option>` text, toasts). */
export function formatProjectLabel(label: ProjectLabel): string {
  return label.prefix ? `${label.prefix}/${label.name}` : label.name;
}

function splitSegments(path: string): string[] {
  return path
    .trim()
    .split(/[\\/]+/)
    .filter((seg) => seg.length > 0);
}

/**
 * Compute display labels for a set of projects, disambiguating duplicate
 * names with the minimal number of trailing path segments — never an
 * ellipsis. Two projects both named `web` at `/work/clients/web` and
 * `/work/personal/web` label as `clients/web` and `personal/web`; the
 * prefix deepens only until the group is distinct.
 *
 * When a project's directory basename equals its display name (the
 * default), the prefix uses parent dirs only, so the name isn't repeated.
 * A renamed project's real dir name differs from the display name and
 * stays in the prefix, since it's part of what tells the copies apart.
 */
export function disambiguatedProjectLabels(
  projects: readonly { id: string; name: string; path: string }[],
): Map<string, ProjectLabel> {
  const out = new Map<string, ProjectLabel>();
  const groups = new Map<string, { id: string; segs: string[] }[]>();
  for (const p of projects) {
    out.set(p.id, { prefix: '', name: p.name });
    const segs = splitSegments(p.path);
    if (segs.length > 0 && segs[segs.length - 1] === p.name) segs.pop();
    const bucket = groups.get(p.name);
    const entry = { id: p.id, segs };
    if (bucket) bucket.push(entry);
    else groups.set(p.name, [entry]);
  }
  for (const members of groups.values()) {
    if (members.length < 2) continue;
    const maxLen = Math.max(...members.map((m) => m.segs.length));
    for (let depth = 1; depth <= maxLen; depth++) {
      const prefixes = members.map((m) => m.segs.slice(-depth).join('/'));
      const distinct = new Set(prefixes).size === members.length;
      // At maxLen we stamp whatever we have even if still tied (only
      // reachable via pathological near-identical paths) — a tied
      // prefix still beats a silently ambiguous bare name.
      if (distinct || depth === maxLen) {
        for (const [i, m] of members.entries()) {
          const label = out.get(m.id);
          if (label) label.prefix = prefixes[i];
        }
        break;
      }
    }
  }
  return out;
}

export function pathBasename(path: string | undefined): string {
  if (!path) return '';
  const trimmed = path.trim().replace(/[\\/]+$/, '');
  if (!trimmed) return '';
  const slash = trimmed.lastIndexOf('/');
  const backslash = trimmed.lastIndexOf('\\');
  const idx = Math.max(slash, backslash);
  return idx >= 0 ? trimmed.slice(idx + 1) : trimmed;
}
