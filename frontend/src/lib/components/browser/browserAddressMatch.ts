// What the browser's address field offers for the text typed into it, from
// three local sources in a fixed order: the thread's other open tabs (taking
// one switches to it), the dev servers the thread runs, and the addresses its
// pages recently opened (internal/browser/recent.go). Nothing here asks the
// network. The typed text stays the user's: a suggestion is taken only when
// chosen, never written into the field.

/** One of the thread's tabs, as the pane's state names it. */
export interface SuggestionTab {
  id: string;
  url: string;
  title: string;
  creating?: boolean;
  error?: string;
}

/** A dev server one of the thread's processes is serving. */
export interface SuggestionDevServer {
  url: string;
  title: string;
}

/** An address one of the thread's pages recently opened. */
export interface SuggestionRecent {
  url: string;
  title?: string;
}

export type AddressSuggestionKind = 'tab' | 'dev-server' | 'recent';

export interface AddressSuggestion {
  kind: AddressSuggestionKind;
  /** Unique within one list. */
  key: string;
  /** What taking it opens; for a tab, the tab's address. */
  address: string;
  title: string;
  /** The tab a 'tab' suggestion switches to. */
  pageId?: string;
}

export interface AddressSuggestionSources {
  tabs: readonly SuggestionTab[];
  /** The tab the field belongs to, which is not offered. */
  activePageId: string;
  devServers: readonly SuggestionDevServer[];
  recent: readonly SuggestionRecent[];
}

/** The most rows each source contributes, and the most the list shows. */
export const SUGGESTION_LIMITS = { tab: 4, 'dev-server': 3, recent: 5, total: 8 } as const;

/**
 * An address as typed text is matched against: no scheme, no `www.`, no
 * trailing slash, any case. Forgiving, so never an identity.
 */
function bare(address: string): string {
  return address
    .toLowerCase()
    .replace(/^[a-z][a-z0-9+.-]*:\/\//, '')
    .replace(/^www\./, '')
    .replace(/\/+$/, '');
}

/**
 * Where an address goes, for telling one destination from another: the URL
 * parser's form, so scheme and host case and an empty path's slash
 * normalize while path, query and fragment stay exact. An address the
 * parser refuses is its exact text.
 */
function destination(address: string): string {
  try {
    return new URL(address).href;
  } catch {
    return address;
  }
}

/**
 * How well a candidate matches: 0 when its address starts with the query,
 * 1 when its address holds every word of it, 2 when address and title
 * together do, null when they do not.
 */
function matchRank(query: string, words: readonly string[], address: string, title: string): number | null {
  const plain = bare(address);
  const full = address.toLowerCase();
  // Typing a scheme is typing the address from its start; otherwise every
  // address would start with an "h".
  if (plain.startsWith(query) || (query.includes('://') && full.startsWith(query))) return 0;
  if (words.every((word) => full.includes(word))) return 1;
  const text = `${full} ${title.toLowerCase()}`;
  return words.every((word) => text.includes(word)) ? 2 : null;
}

interface Candidate {
  suggestion: AddressSuggestion;
  rank: number;
  order: number;
}

function ranked(candidates: Candidate[], limit: number): AddressSuggestion[] {
  return candidates
    .sort((a, b) => a.rank - b.rank || a.order - b.order)
    .slice(0, limit)
    .map((candidate) => candidate.suggestion);
}

/** The suggestions for `typed`, in source order; empty for blank text. */
export function addressSuggestions(typed: string, sources: AddressSuggestionSources): AddressSuggestion[] {
  const query = typed.trim().toLowerCase();
  if (query === '') return [];
  const words = query.split(/\s+/);
  // An address already offered by an earlier source is not offered again.
  const offered = new Set<string>();
  const collect = (
    kind: AddressSuggestionKind,
    entries: readonly { address: string; title: string; pageId?: string }[],
  ): AddressSuggestion[] => {
    const candidates: Candidate[] = [];
    const seen = new Set<string>();
    entries.forEach((entry, order) => {
      const id = destination(entry.address);
      if (offered.has(id) || seen.has(id)) return;
      seen.add(id);
      const rank = matchRank(query, words, entry.address, entry.title);
      if (rank === null) return;
      candidates.push({
        suggestion: { kind, key: `${kind}:${entry.pageId ?? entry.address}`, address: entry.address, title: entry.title, pageId: entry.pageId },
        rank,
        order,
      });
    });
    const chosen = ranked(candidates, SUGGESTION_LIMITS[kind]);
    for (const suggestion of chosen) offered.add(destination(suggestion.address));
    return chosen;
  };

  // The field's own tab is what the text replaces, so it is no destination,
  // and neither is its address under another source.
  const active = sources.tabs.find((tab) => tab.id === sources.activePageId);
  if (active?.url) offered.add(destination(active.url));
  const tabs = collect(
    'tab',
    sources.tabs
      .filter((tab) => tab.id !== sources.activePageId && !tab.creating && !tab.error && tab.url !== '' && tab.url !== 'about:blank')
      .map((tab) => ({ address: tab.url, title: tab.title, pageId: tab.id })),
  );
  const devServers = collect('dev-server', sources.devServers.map((server) => ({ address: server.url, title: server.title })));
  const recent = collect('recent', sources.recent.map((entry) => ({ address: entry.url, title: entry.title ?? '' })));
  return [...tabs, ...devServers, ...recent].slice(0, SUGGESTION_LIMITS.total);
}

/** A dev server row as the store lists it (internal/devscan.DevServer). */
export interface DevServerRow {
  port: number;
  threadId?: string;
  process?: string;
  listening: boolean;
  scheme?: string;
}

/** The thread's own listening dev servers, as addresses on this machine. */
export function threadDevServers(threadId: string, servers: readonly DevServerRow[]): SuggestionDevServer[] {
  const out: SuggestionDevServer[] = [];
  for (const server of servers) {
    if (server.threadId !== threadId || !server.listening) continue;
    const scheme = server.scheme === 'https' ? 'https' : 'http';
    out.push({
      url: `${scheme}://localhost:${server.port}/`,
      title: server.process ? `${server.process} on port ${server.port}` : `Port ${server.port}`,
    });
  }
  return out;
}
