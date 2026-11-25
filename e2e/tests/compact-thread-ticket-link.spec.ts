// A thread's ticket link on the compact project; the flow and its coverage
// are in thread-ticket-link-flow.ts (thread-ticket-link.spec.ts is the
// desktop twin).
import { registerThreadTicketLinkFlow } from './thread-ticket-link-flow.js';

registerThreadTicketLinkFlow({ compact: true });
