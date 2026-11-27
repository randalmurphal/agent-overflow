// The Dashboards screen under the compact layout; the flow and its
// coverage are in issues-dashboards-flow.ts, issues-dashboards-agent-flow.ts,
// issues-dashboards-list-flow.ts and issues-board-row-flow.ts (issues-dashboards.spec.ts is the
// desktop twin).
import { registerBoardRowFlow } from './issues-board-row-flow.js';
import { registerDashboardAgentFlow } from './issues-dashboards-agent-flow.js';
import { registerDashboardsFlow } from './issues-dashboards-flow.js';
import { registerDashboardListFlow } from './issues-dashboards-list-flow.js';

registerDashboardsFlow({ compact: true });
registerDashboardAgentFlow({ compact: true });
registerDashboardListFlow({ compact: true });
registerBoardRowFlow({ compact: true });
