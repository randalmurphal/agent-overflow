// mod+f filters the Pull requests page's rows, on the compact project; the
// flow and its coverage are in pulls-page-find-flow.ts.
import { registerPullsPageFindFlow } from './pulls-page-find-flow.js';

registerPullsPageFindFlow({ compact: true });
