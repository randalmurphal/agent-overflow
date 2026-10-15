// The unresolved stepper and the jump pulse on the compact layout: taps,
// the 32px control, the review pane reached through the header's more
// menu. A touch screen has no hover, so the hover checks are the desktop
// project's. Coverage is review-unresolved-stepper-flow.ts's header.
import { unresolvedStepperFlow } from './review-unresolved-stepper-flow.js';

unresolvedStepperFlow({ compact: true });
