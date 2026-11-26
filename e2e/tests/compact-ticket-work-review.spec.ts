// A ticket's Work pull request shown in the app on the compact project;
// the flow and its coverage are in ticket-work-review-flow.ts
// (ticket-work-review.spec.ts is the desktop twin).
import { registerTicketWorkReviewFlow } from './ticket-work-review-flow.js';

registerTicketWorkReviewFlow({ compact: true });
