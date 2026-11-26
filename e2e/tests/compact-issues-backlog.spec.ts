// The Backlog screen under the compact layout; the flow and its coverage
// are in issues-backlog-flow.ts (issues-backlog.spec.ts is the desktop
// twin).
import { registerBacklogFlow } from './issues-backlog-flow.js';

registerBacklogFlow({ compact: true });
