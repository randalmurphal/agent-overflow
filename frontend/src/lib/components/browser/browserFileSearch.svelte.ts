// The browser file picker's search: the workspace file search the
// composer's @-mention runs (SearchWorkspaceFiles), debounced, newest query
// wins. Files only; a directory is opened in the folder view instead.
// `results` are always the answer to the query on screen: while a changed
// query is pending there are none, so nothing stale can be chosen.

import { SearchWorkspaceFiles } from '../../stores/bindings';
import type { WorkspaceRef } from '../../types/git';
import type { WorkspaceFileSearchResult } from '../../types/workspaceFile';
import { errString } from '../../utils/errors';

export const BROWSER_FILE_SEARCH_DEBOUNCE_MS = 120;
const LIMIT = 50;

export interface BrowserFileHit {
  /** Workspace-relative, as listed. */
  path: string;
  /** Absolute on the workspace's computer, what the browser opens. */
  absolute: string;
}

export interface BrowserFileSearch {
  readonly query: string;
  readonly results: BrowserFileHit[];
  readonly loading: boolean;
  readonly error: string;
  readonly truncated: boolean;
  setQuery(query: string): void;
  dispose(): void;
}

export function joinWorkspacePath(root: string, relative: string): string {
  return root.endsWith('/') ? root + relative : `${root}/${relative}`;
}

interface Answer {
  query: string;
  hits: BrowserFileHit[];
  truncated: boolean;
  error: string;
}

const NO_ANSWER: Answer = { query: '', hits: [], truncated: false, error: '' };

export function createBrowserFileSearch(workspace: () => WorkspaceRef | null): BrowserFileSearch {
  let query = $state('');
  // The last answer, keyed by the trimmed query it answers.
  let answered = $state<Answer>(NO_ANSWER);
  let loading = $state(false);
  const current = $derived(answered.query === query.trim() ? answered : NO_ANSWER);
  let generation = 0;
  let timer: ReturnType<typeof setTimeout> | null = null;

  async function run(text: string, token: number): Promise<void> {
    const ws = workspace();
    if (!ws) {
      answered = { ...NO_ANSWER, query: text, error: 'This conversation has no workspace to search.' };
      loading = false;
      return;
    }
    try {
      const result = (await SearchWorkspaceFiles(ws, text, LIMIT)) as WorkspaceFileSearchResult;
      if (token !== generation) return;
      const hits = (result?.files ?? [])
        .filter((file) => file.kind === 'file')
        .map((file) => ({ path: file.path, absolute: joinWorkspacePath(result.root, file.path) }));
      answered = { query: text, hits, truncated: result?.truncated === true, error: '' };
    } catch (err) {
      if (token !== generation) return;
      answered = { ...NO_ANSWER, query: text, error: errString(err) };
    } finally {
      if (token === generation) loading = false;
    }
  }

  return {
    get query() { return query; },
    get results() { return current.hits; },
    get loading() { return loading; },
    get error() { return current.error; },
    get truncated() { return current.truncated; },
    setQuery(next: string) {
      query = next;
      const token = ++generation;
      if (timer) clearTimeout(timer);
      timer = null;
      const text = next.trim();
      if (!text) {
        loading = false;
        return;
      }
      loading = true;
      timer = setTimeout(() => {
        timer = null;
        void run(text, token);
      }, BROWSER_FILE_SEARCH_DEBOUNCE_MS);
    },
    dispose() {
      generation++;
      if (timer) clearTimeout(timer);
      timer = null;
    },
  };
}
