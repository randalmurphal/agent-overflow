import { createThreadPane, type ThreadPane } from './thread.svelte';

// Active panes, keyed by pane ID. v1 has exactly one pane ("main").
let panes: Map<string, ThreadPane> = $state(new Map());

export function getMainPane(): ThreadPane {
  let main = panes.get('main');
  if (!main) {
    main = createThreadPane();
    panes = new Map(panes).set('main', main);
  }
  return main;
}

export function getAllPanes(): Map<string, ThreadPane> {
  return panes;
}
