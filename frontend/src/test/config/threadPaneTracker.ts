// Records every pane `createThreadPane` makes in the happy-dom projects, so
// the between-test steps (src/test/openThreadPanes.ts) can clear the ones a
// test left open. The record is appended to the store's compiled module, so
// the app's code carries no test hook.
import { resolve } from 'node:path';
import type { Plugin } from 'vite';

export const OPEN_THREAD_PANES_KEY = 'agent-overflow.test.openThreadPanes';

export function threadPaneTracker(root: string): Plugin {
  const threadStore = resolve(root, 'src/lib/stores/thread.svelte.ts');
  const record = `
;{
  const createUntracked = createThreadPane;
  const open = (globalThis[Symbol.for(${JSON.stringify(OPEN_THREAD_PANES_KEY)})] ??= []);
  createThreadPane = (...args) => {
    const pane = createUntracked(...args);
    open.push(pane);
    return pane;
  };
}
`;
  return {
    name: 'thread-pane-tracking',
    enforce: 'post',
    transform(code, id) {
      // Not a query of the module, such as its `?raw` source text.
      if (id !== threadStore) return null;
      return { code: code + record, map: null };
    },
  };
}
