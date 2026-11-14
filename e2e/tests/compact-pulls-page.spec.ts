// The Pull requests page on the compact project: the list is the page, an
// opened merge request or pipeline is full width with Back; the flow and
// its coverage are in pulls-page-flow.ts.
import { registerPullsPageFlow } from './pulls-page-flow.js';

registerPullsPageFlow({ compact: true });
