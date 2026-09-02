// The dev-server preview gateway under the phone descriptor, which is the
// device the whole feature exists for: a `localhost:<port>` an agent
// printed on a desktop, read on a phone that cannot reach that port at
// all (spec §7, the port gateway).
//
// The same suite as `preview-gateway.spec.ts`, from the same module. Only
// two moves differ under the compact layout and both are the surface's,
// not the flow's: the thread screen and the list screen are one mounted
// tree with a visibility flip, and Settings is reached from the sidebar,
// which is inert while the thread is showing.

import { COMPACT_SURFACE, definePreviewGatewaySuite } from './preview-gateway-helpers.js';

definePreviewGatewaySuite(COMPACT_SURFACE);
