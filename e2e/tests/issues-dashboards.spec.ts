// The Dashboards screen on the desktop project; the flow and its coverage
// are in issues-dashboards-flow.ts, for the widgets agents propose in
// issues-dashboards-agent-flow.ts, for a ticket list's selection in
// issues-dashboards-list-flow.ts, and for a widget's limit and pin and
// the board's summary row in issues-board-row-flow.ts (compact-issues-dashboards.spec.ts is
// the phone twin).
import { registerBoardRowFlow } from './issues-board-row-flow.js';
import { registerDashboardAgentFlow } from './issues-dashboards-agent-flow.js';
import { registerDashboardsFlow } from './issues-dashboards-flow.js';
import { registerDashboardListFlow } from './issues-dashboards-list-flow.js';

registerDashboardsFlow({ compact: false });
registerDashboardAgentFlow({ compact: false });
registerDashboardListFlow({ compact: false });
registerBoardRowFlow({ compact: false });
