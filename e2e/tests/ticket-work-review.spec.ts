// A ticket's Work pull request shown in the app on the desktop project;
// the flow and its coverage are in ticket-work-review-flow.ts
// (compact-ticket-work-review.spec.ts is the phone twin).
import { registerTicketWorkReviewFlow } from './ticket-work-review-flow.js';

registerTicketWorkReviewFlow({ compact: false });
