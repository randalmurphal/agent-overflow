// A project's status groups and ticket pins under the compact layout; the
// flow and its coverage are in issues-setup-flow.ts (issues-setup.spec.ts
// is the desktop twin).
import { registerIssuesSetupFlow } from './issues-setup-flow.js';

registerIssuesSetupFlow({ compact: true });
