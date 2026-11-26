// The tracker tools' writes on the desktop project; the flow and its
// coverage are in tracker-agent-writes-flow.ts
// (compact-tracker-agent-writes.spec.ts is the phone twin).
import { registerTrackerAgentWritesFlow } from './tracker-agent-writes-flow.js';

registerTrackerAgentWritesFlow({ compact: false });
