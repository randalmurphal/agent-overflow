// Presentation of each tool pane kind, shared by the pane header and the
// sidebar's tools row so the two cannot name a tool differently. Order is
// the tools row's order.
import type { Component } from 'svelte';
import GitPullRequestArrow from '@lucide/svelte/icons/git-pull-request-arrow';
import Kanban from '@lucide/svelte/icons/kanban';
import Workflow from '@lucide/svelte/icons/workflow';
import type { ToolPaneKind } from '../../stores/paneLayout.svelte';
import type { Scope } from '../../transport/scopes';

export interface ToolPaneMeta {
  kind: ToolPaneKind;
  title: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  icon: Component<any>;
  /** The tools row lists the tool only while an attached computer grants
   * this scope. */
  scope?: Exclude<Scope, 'host'>;
}

export const TOOL_PANES: readonly ToolPaneMeta[] = [
  { kind: 'pulls', title: 'Pull requests', icon: GitPullRequestArrow },
  { kind: 'workflows', title: 'Workflows', icon: Workflow },
  { kind: 'issues', title: 'Issues', icon: Kanban, scope: 'tracker:read' },
];

export function toolPaneMeta(kind: ToolPaneKind): ToolPaneMeta {
  const meta = TOOL_PANES.find((entry) => entry.kind === kind);
  if (!meta) throw new Error(`Unknown tool pane kind "${kind}".`);
  return meta;
}
