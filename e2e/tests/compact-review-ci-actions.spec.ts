// CI actions from the Pipelines tab on the compact project, where a job
// row's actions show without hover; the flow and its coverage are in
// ci-actions-flow.ts (review-ci-actions.spec.ts is the desktop twin).
import { registerCIActionsFlow } from './ci-actions-flow.js';

registerCIActionsFlow({ compact: true });
