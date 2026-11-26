// An agent's post of drafted time on the desktop project; the flow and its
// coverage are in tracker-time-post-flow.ts (compact-tracker-time-post.spec.ts
// is the phone twin).
import { registerTimePostFlow } from './tracker-time-post-flow.js';

registerTimePostFlow({ compact: false });
