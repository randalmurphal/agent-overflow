// The Dashboards screen on the desktop project; the flow and its coverage
// are in issues-dashboards-flow.ts, for the widgets agents propose in
// issues-dashboards-agent-flow.ts, and for a ticket list's selection in
// issues-dashboards-list-flow.ts (compact-issues-dashboards.spec.ts is
// the phone twin).
import { registerDashboardAgentFlow } from './issues-dashboards-agent-flow.js';
import { registerDashboardsFlow } from './issues-dashboards-flow.js';
import { registerDashboardListFlow } from './issues-dashboards-list-flow.js';

registerDashboardsFlow({ compact: false });
registerDashboardAgentFlow({ compact: false });
registerDashboardListFlow({ compact: false });
