// A ticket's Work, Ask and Start work on the desktop project; the flow and
// its coverage are in ticket-work-flow.ts (compact-ticket-work.spec.ts is
// the phone twin).
import { registerTicketWorkFlow } from './ticket-work-flow.js';

registerTicketWorkFlow({ compact: false });
