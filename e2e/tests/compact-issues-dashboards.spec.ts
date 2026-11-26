// The Dashboards screen under the compact layout; the flow and its
// coverage are in issues-dashboards-flow.ts (issues-dashboards.spec.ts is
// the desktop twin).
import { registerDashboardsFlow } from './issues-dashboards-flow.js';

registerDashboardsFlow({ compact: true });
