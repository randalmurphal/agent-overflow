// Changing tickets from the Board screen on the desktop project; the flow
// and its coverage are in issues-board-writes-flow.ts
// (compact-issues-board-writes.spec.ts is the phone twin).
import { registerBoardWritesFlow } from './issues-board-writes-flow.js';

registerBoardWritesFlow({ compact: false });
