// A new comment's editor across a change to its file, on the compact
// layout, where Back also cancels an armed Move. Coverage:
// review-editor-relocation-flow.ts; desktop: review-editor-relocation.spec.ts.
import { test } from './fixtures.js';
import { reviewEditorRelocationFlow } from './review-editor-relocation-flow.js';

// Screenshots of the states are read at pixel ratio 1.
if (process.env.AO_REVIEW_SHOTS) test.use({ deviceScaleFactor: 1 });

reviewEditorRelocationFlow({ compact: true });
