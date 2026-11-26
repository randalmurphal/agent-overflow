// The Dashboards screen on the desktop project; the flow and its coverage
// are in issues-dashboards-flow.ts and, for the widgets agents propose,
// issues-dashboards-agent-flow.ts (compact-issues-dashboards.spec.ts is
// the phone twin).
import { registerDashboardAgentFlow } from './issues-dashboards-agent-flow.js';
import { registerDashboardsFlow } from './issues-dashboards-flow.js';

registerDashboardsFlow({ compact: false });
registerDashboardAgentFlow({ compact: false });
