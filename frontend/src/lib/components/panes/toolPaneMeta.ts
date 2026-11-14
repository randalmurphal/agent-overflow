// Presentation of each tool pane kind, shared by the pane header and the
// sidebar's tools row so the two cannot name a tool differently. Order is
// the tools row's order.
import type { Component } from 'svelte';
import GitPullRequestArrow from '@lucide/svelte/icons/git-pull-request-arrow';
import Workflow from '@lucide/svelte/icons/workflow';
import type { ToolPaneKind } from '../../stores/paneLayout.svelte';

export interface ToolPaneMeta {
  kind: ToolPaneKind;
  title: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  icon: Component<any>;
}

export const TOOL_PANES: readonly ToolPaneMeta[] = [
  { kind: 'pulls', title: 'Pull requests', icon: GitPullRequestArrow },
  { kind: 'workflows', title: 'Workflows', icon: Workflow },
];

export function toolPaneMeta(kind: ToolPaneKind): ToolPaneMeta {
  const meta = TOOL_PANES.find((entry) => entry.kind === kind);
  if (!meta) throw new Error(`Unknown tool pane kind "${kind}".`);
  return meta;
}
