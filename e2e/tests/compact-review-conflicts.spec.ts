// The review pane's conflict view on the compact layout, where the review
// companion is the one pane on screen and so keeps its reconcile permit.
// Coverage is review-conflicts-flow.ts's header.
import { reviewConflictsFlow } from './review-conflicts-flow.js';

reviewConflictsFlow({ compact: true });
