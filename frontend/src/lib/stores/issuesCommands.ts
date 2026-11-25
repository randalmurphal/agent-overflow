// The Issues pane's command: open or reveal the pane, or close it when it
// is focused, while this window may use the pane (`trackerRead`). It has
// no default chord.

import { registerCommand } from './commandRegistry.svelte';
import { toggleTool } from './toolPanes.svelte';

export function registerIssuesCommands(): void {
  registerCommand({
    id: 'issues.toggle',
    label: 'Issues: Open',
    description: 'Open or reveal the Issues pane. Running it while the pane is focused closes it.',
    icon: '▦',
    when: 'trackerRead',
    run: () => toggleTool('issues'),
  });
}
