// The unresolved stepper and the jump pulse on the desktop layout, with a
// mouse: hover tints stay inside the control's border. Coverage is
// review-unresolved-stepper-flow.ts's header. Screenshots render at 2x so
// a 24px control can be inspected.
import { test } from './fixtures.js';
import { unresolvedStepperFlow } from './review-unresolved-stepper-flow.js';

test.use({ deviceScaleFactor: 2 });

unresolvedStepperFlow({ compact: false });
