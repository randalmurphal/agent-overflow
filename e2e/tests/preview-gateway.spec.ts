// The dev-server preview gateway on the desktop layout: an agent's
// `http://localhost:<port>` read from a paired browser that is not on the
// machine that printed it, allowed by hand, opened through the port
// gateway, and then revoked and unshared (spec §7, the port gateway).
//
// The flow, the fake dev server it is driven against, and why it owns its
// backend all live in `preview-gateway-helpers.ts`; the compact project
// runs the same suite from `compact-preview-gateway.spec.ts`. This file is
// the desktop half of that pair and deliberately nothing else, so the two
// projects cannot drift into proving different things.

import { DESKTOP_SURFACE, definePreviewGatewaySuite } from './preview-gateway-helpers.js';

definePreviewGatewaySuite(DESKTOP_SURFACE);
