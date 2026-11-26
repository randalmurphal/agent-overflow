// A ticket's Work, Ask and Start work on the compact project; the flow and
// its coverage are in ticket-work-flow.ts (ticket-work.spec.ts is the
// desktop twin).
import { registerTicketWorkFlow } from './ticket-work-flow.js';

registerTicketWorkFlow({ compact: true });
