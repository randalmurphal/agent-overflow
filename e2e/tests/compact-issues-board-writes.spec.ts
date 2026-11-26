// Changing tickets from the Board screen under the compact layout; the
// flow and its coverage are in issues-board-writes-flow.ts
// (issues-board-writes.spec.ts is the desktop twin).
import { registerBoardWritesFlow } from './issues-board-writes-flow.js';

registerBoardWritesFlow({ compact: true });
