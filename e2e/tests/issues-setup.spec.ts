// A project's status groups and ticket pins on the desktop project; the
// flow and its coverage are in issues-setup-flow.ts
// (compact-issues-setup.spec.ts is the phone twin).
import { registerIssuesSetupFlow } from './issues-setup-flow.js';

registerIssuesSetupFlow({ compact: false });
