// The tracker tools' writes on the compact project: the prompt in the
// composer, the timeline row's key opening the ticket full width, and a
// worker's prompt reached from its spawner's Agents row; the flow and its
// coverage are in tracker-agent-writes-flow.ts.
import { registerTrackerAgentWritesFlow } from './tracker-agent-writes-flow.js';

registerTrackerAgentWritesFlow({ compact: true });
