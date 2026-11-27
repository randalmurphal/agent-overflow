// The activity recorder on the desktop project; the flow and its coverage
// are in tracker-activity-flow.ts (compact-tracker-activity.spec.ts is the
// phone twin).
import { registerActivityFlow } from './tracker-activity-flow.js';

registerActivityFlow({ compact: false });
