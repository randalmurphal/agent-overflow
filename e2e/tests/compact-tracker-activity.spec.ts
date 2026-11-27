// The activity recorder on the compact project, the phone's minutes
// included; the flow and its coverage are in tracker-activity-flow.ts.
import { registerActivityFlow } from './tracker-activity-flow.js';

registerActivityFlow({ compact: true });
